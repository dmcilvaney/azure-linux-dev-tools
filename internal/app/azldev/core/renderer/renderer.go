// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package renderer renders the post-overlay spec and sidecar files for one
// or more components into a checked-in output directory.
//
// The pipeline runs in three phases:
//
//  1. Parallel source preparation (clone, overlay, synthetic git)         [render_phase_prepare.go]
//  2. Batch mock processing (rpmautospec + spectool in one chroot call)   [render_phase_mock.go]
//  3. Parallel finishing (filter files, remove .git, diff, copy output)   [render_phase_finish.go]
//
// Supporting concerns are factored into their own files:
//
//   - renderer.go (this file)  — public entrypoint [Render], the [Options] /
//     [Result] types, and the intermediate types
//     ([preparedComponent], [prepResult]) passed
//     between phases.
//   - render_options.go        — output-dir resolution, validation, mock
//     processor construction.
//   - render_result.go         — [Result] type, status consts, sort, the
//     error / check-only summarizers.
//   - render_marker.go         — RENDER_FAILED marker contract (write,
//     read, verify).
//   - render_orphans.go        — orphan dir detection and pruning for
//     --clean-stale runs.
//
// The CLI wrapper lives at
// internal/app/azldev/cmds/component/render.go and translates cobra flags
// into an [Options] before calling [Render].
package renderer

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// Options holds the inputs for [Render]. Populated by the CLI from cobra
// flags or by an in-process caller (e.g. update → render handoff) directly.
type Options struct {
	ComponentFilter   components.ComponentFilter
	OutputDir         string
	OutputDirExplicit bool // True when --output-dir was explicitly passed on the CLI.
	FailOnError       bool
	Force             bool
	CleanStale        bool
	CheckOnly         bool
}

// preparedComponent holds the intermediate state after source preparation,
// before mock processing. Internal to the renderer package; phases 1, 2,
// and 3 all consume it.
type preparedComponent struct {
	index         int
	comp          components.Component
	specFilename  string // e.g., "curl.spec"
	compOutputDir string // validated output path computed in phase 1
}

// prepResult pairs a prepared component (on success) or a render result
// (on error). Returned from each phase-1 worker to its caller.
type prepResult struct {
	prepared *preparedComponent
	result   *Result // non-nil on error
}

// Render renders the post-overlay spec and sidecar files for each selected
// component into the output directory. See the package doc for the three
// phases.
func Render(env *azldev.Env, options *Options) ([]*Result, error) {
	if err := resolveAndValidateOutputDir(env, options); err != nil {
		return nil, err
	}

	if err := validateCleanStaleOptions(options); err != nil {
		return nil, err
	}

	resolver := components.NewResolver(env)

	comps, err := resolver.FindComponents(&options.ComponentFilter)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve components:\n%w", err)
	}

	if comps.Len() == 0 {
		return nil, errors.New("no components were selected; " +
			"please use command-line options to indicate which components to render",
		)
	}

	// Create mock processor for rpmautospec/spectool.
	mockProcessor := createMockProcessor(env)
	if mockProcessor == nil {
		return nil, errors.New(
			"mock config required for rendering; ensure the project has a valid distro with mock config")
	}

	defer mockProcessor.Destroy(env)

	// Create a shared staging directory. Each component gets a subdirectory
	// named by component name, enabling a single bind mount for the batch
	// mock invocation. Use the project work dir instead of /tmp to avoid
	// filling up tmpfs on large renders.
	if err := env.FS().MkdirAll(env.WorkDir(), fileperms.PublicDir); err != nil {
		return nil, fmt.Errorf("creating work directory:\n%w", err)
	}

	stagingDir, err := fileutils.MkdirTemp(env.FS(), env.WorkDir(), "azldev-render-staging-")
	if err != nil {
		return nil, fmt.Errorf("creating staging directory:\n%w", err)
	}

	defer func() {
		if removeErr := env.FS().RemoveAll(stagingDir); removeErr != nil {
			slog.Debug("Failed to clean up staging directory", "path", stagingDir, "error", removeErr)
		}
	}()

	componentList := comps.Components()
	results := make([]*Result, len(componentList))

	// ── Phase 1: Parallel source preparation ──
	prepared := parallelPrepare(env, componentList, stagingDir, options.OutputDir, results)

	// ── Phase 2: Batch mock processing ──
	mockResultMap := batchMockProcess(env, mockProcessor, stagingDir, prepared)

	// Prune orphan component dirs (components removed from config) when
	// --clean-stale is set. Per-component output dirs that match the resolved
	// component set are NOT touched here; phase 3 will RemoveAll+rewrite each
	// of them via copyRenderedOutput. This keeps the diff against existing
	// output meaningful (so result.Changed reflects actual content drift
	// instead of being unconditionally true after a blanket wipe) and reduces
	// the blast radius of a Ctrl-C: we only ever delete dirs that wouldn't
	// have been re-rendered anyway.
	//
	// Skipped in --check-only mode -- check-only must never touch disk; the
	// orphan list is computed read-only later via checkOnlyRenderResult.
	if options.CleanStale && !options.CheckOnly {
		names := make([]string, len(componentList))
		for idx, comp := range componentList {
			names[idx] = comp.GetName()
		}

		if pruneErr := pruneOrphanRenderedDirs(env.FS(), options.OutputDir, names); pruneErr != nil {
			return nil, fmt.Errorf("pruning orphan rendered-spec dirs in %#q:\n%w", options.OutputDir, pruneErr)
		}
	}

	// ── Phase 3: Parallel finishing ──
	parallelFinish(env, prepared, mockResultMap, results, stagingDir,
		options.Force, options.CheckOnly)

	// Write RENDER_FAILED markers for any component that errored in phase 1
	// (source preparation) or phase 3 (mock result application + copy).
	// Centralizing this here makes it idempotent with the --clean-stale wipe
	// (which sits between phases 2 and 3) and keeps the per-phase code free
	// of bookkeeping. In --check-only mode this verifies that on-disk state
	// matches the expected single-marker shape and flags drift on mismatch.
	writeFailureMarkers(env.FS(), results, options.Force, options.CheckOnly)

	// Sort results alphabetically for consistent output.
	sortRenderResults(results)

	if options.CheckOnly {
		return results, checkOnlyRenderResult(env.FS(), options, componentList, results)
	}

	return results, checkRenderErrors(results, options.FailOnError)
}
