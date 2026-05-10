// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
)

// parallelPrepare prepares sources for all components concurrently, bounded by
// [azldev.Env.IOBoundConcurrency]. Each component's sources are written to a
// subdirectory of stagingDir. Failed and cancelled components get their
// result written directly into results; successful ones are returned in the
// prepared slice for phase 2 / phase 3.
func parallelPrepare(
	env *azldev.Env,
	comps []components.Component,
	stagingDir string,
	outputDir string,
	results []*Result,
) []*preparedComponent {
	progressEvent := env.StartEvent("Preparing component sources", "count", len(comps))
	defer progressEvent.End()

	workerEnv, cancel := env.WithCancel()
	defer cancel()

	total := int64(len(comps))

	parmapResults := parmap.Map(
		workerEnv,
		env.IOBoundConcurrency(),
		comps,
		func(done, _ int) { progressEvent.SetProgress(int64(done), total) },
		func(_ context.Context, comp components.Component) prepResult {
			// workerEnv (captured) is the effective context for this call chain;
			// the parmap-supplied ctx is identical and unused here.
			return prepareOneComponent(workerEnv, comp, stagingDir, outputDir) //nolint:contextcheck // env carries the ctx
		},
	)

	prepared := make([]*preparedComponent, 0, len(comps))

	for idx, result := range parmapResults {
		switch {
		case result.Cancelled:
			// Worker never started — ctx ended before parmap reached it.
			compName := comps[idx].GetName()

			compOutputDir, nameErr := components.RenderedSpecDir(outputDir, compName)
			if nameErr != nil {
				compOutputDir = "(invalid)"
			}

			results[idx] = &Result{
				Component: compName,
				OutputDir: compOutputDir,
				Status:    renderStatusCancelled,
				Error:     "context cancelled",
			}
		case result.Value.result != nil:
			results[idx] = result.Value.result
		default:
			result.Value.prepared.index = idx
			prepared = append(prepared, result.Value.prepared)
		}
	}

	return prepared
}

// prepareOneComponent validates the output path for a single component and
// prepares its sources. Returns a [prepResult] carrying either a successful
// preparedComponent or a [Result] describing the error.
//
// Called from a [parmap.Map] worker; semaphore acquisition and ctx-aware
// cancellation are handled by parmap. Errors from [prepareComponentSources]
// (including ctx cancellation mid-flight) surface as [renderStatusError] here.
func prepareOneComponent(
	env *azldev.Env,
	comp components.Component,
	stagingDir string,
	outputDir string,
) prepResult {
	componentName := comp.GetName()

	// Validate component name and compute output directory.
	compOutputDir, nameErr := components.RenderedSpecDir(outputDir, componentName)
	if nameErr != nil {
		return prepResult{result: &Result{
			Component: componentName,
			OutputDir: "(invalid)",
			Status:    renderStatusError,
			Error:     nameErr.Error(),
		}}
	}

	prep, err := prepareComponentSources(env, comp, stagingDir)
	if err != nil {
		slog.Error("Failed to prepare component sources",
			"component", componentName, "error", err)

		return prepResult{result: &Result{
			Component: componentName,
			OutputDir: compOutputDir,
			Status:    renderStatusError,
			Error:     err.Error(),
		}}
	}

	prep.compOutputDir = compOutputDir

	return prepResult{prepared: prep}
}

// prepareComponentSources resolves the distro, creates a source manager, and
// prepares sources (clone + overlays + synthetic git) for a single component
// into a subdirectory of stagingDir.
func prepareComponentSources(
	env *azldev.Env,
	comp components.Component,
	stagingDir string,
) (*preparedComponent, error) {
	componentName := comp.GetName()

	event := env.StartEvent("Preparing component sources", "component", componentName)
	defer event.End()

	// Resolve the effective distro for this component.
	distro, err := sourceproviders.ResolveDistro(env, comp)
	if err != nil {
		return nil, fmt.Errorf("resolving distro for %#q:\n%w", componentName, err)
	}

	// Create source manager.
	sourceManager, err := sourceproviders.NewSourceManager(env, distro)
	if err != nil {
		return nil, fmt.Errorf("creating source manager for %#q:\n%w", componentName, err)
	}

	// Component sources go into stagingDir/<componentName>/.
	componentDir := filepath.Join(stagingDir, componentName)

	if mkdirErr := fileutils.MkdirAll(env.FS(), componentDir); mkdirErr != nil {
		return nil, fmt.Errorf("creating component staging directory:\n%w", mkdirErr)
	}

	// Prepare sources with overlays, skipping lookaside downloads.
	// WithGitRepo preserves upstream .git and creates synthetic history so
	// rpmautospec can expand %autorelease and %autochangelog correctly.
	// WithSkipLookaside avoids expensive tarball downloads — only spec +
	// sidecar files are needed for rendering.
	preparerOpts := []sources.PreparerOption{
		sources.WithGitRepo(env, env.LockReader()),
		sources.WithSkipLookaside(),
	}

	preparer, err := sources.NewPreparer(sourceManager, env.FS(), env, env, preparerOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating source preparer for %#q:\n%w", componentName, err)
	}

	if prepErr := preparer.PrepareSources(env, comp, componentDir, true /*applyOverlays*/); prepErr != nil {
		return nil, fmt.Errorf("preparing sources for %#q:\n%w", componentName, prepErr)
	}

	// Find the spec file so we can pass the filename to mock.
	specPath, specErr := findSpecFile(env.FS(), componentDir, componentName)
	if specErr != nil {
		return nil, fmt.Errorf("finding spec file for %#q:\n%w", componentName, specErr)
	}

	return &preparedComponent{
		comp:         comp,
		specFilename: filepath.Base(specPath),
	}, nil
}

// findSpecFile locates the spec file for a component in the given directory.
func findSpecFile(fs opctx.FS, dir, componentName string) (string, error) {
	specPath := filepath.Join(dir, componentName+".spec")

	exists, err := fileutils.Exists(fs, specPath)
	if err != nil {
		return "", fmt.Errorf("checking spec file %#q:\n%w", specPath, err)
	}

	if !exists {
		return "", fmt.Errorf("expected spec file %#q not found for component %#q", specPath, componentName)
	}

	return specPath, nil
}
