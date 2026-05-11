// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package renderer renders the post-overlay spec and sidecar files for one
// or more components into a checked-in output directory.
//
// One component = one [renderJob], threaded through the pipeline via three
// view interfaces that the type system enforces in order:
//
//	NewJob ─Prepare/MarkCancelled─▶ PreparedJob ─ApplyMockResult─▶ FinishedJob
//
// The orchestrator runs three batched-parallel phases between the views:
//
//  1. parallelPrepare  — clone, overlay, synthetic git              [render_phase_prepare.go]
//     []NewJob → []PreparedJob
//  2. batchMockProcess — rpmautospec + spectool in one chroot call  [render_phase_mock.go]
//  3. parallelFinish   — filter files, strip .git                   [render_phase_finish.go]
//     []PreparedJob → []FinishedJob
//
// Then serially: Diff every finished job against disk (always), and unless
// --check-only, Apply each one (copy content / write failure marker / no-op
// for cancelled).
//
// Supporting files:
//
//   - renderer.go (this file)  — public entrypoint [Render], [Options],
//     package doc.
//   - job.go                   — [Job]/[NewJob]/[PreparedJob]/[FinishedJob]
//     interfaces + the concrete [renderJob]
//     implementing all three.
//   - render_options.go        — output-dir resolution + validation.
//   - render_result.go         — [Result] type + status helpers.
//   - render_marker.go         — RENDER_FAILED marker contract.
//   - render_orphans.go        — orphan dir detection / pruning.
//
// The CLI wrapper lives at internal/app/azldev/cmds/component/render.go.
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

// Render runs the full render pipeline. See the package doc for the phases.
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

	mockProcessor := createMockProcessor(env)
	if mockProcessor == nil {
		return nil, errors.New(
			"mock config required for rendering; ensure the project has a valid distro with mock config")
	}

	defer mockProcessor.Destroy(env)

	stagingDir, cleanup, err := createStagingDir(env)
	if err != nil {
		return nil, err
	}

	defer cleanup()

	componentList := comps.Components()

	// Build one renderJob per component up front. The job carries every
	// piece of state (paths, errors, change-detection) through the pipeline.
	newJobs := buildJobs(componentList, stagingDir, options.OutputDir)

	// ── Phase 1 → []PreparedJob ──
	preparedJobs := parallelPrepare(env, newJobs)

	// ── Phase 2: batched mock invocation ──
	mockResultMap := batchMockProcess(env, mockProcessor, stagingDir, preparedJobs)

	// Prune orphan component dirs (components removed from config) when
	// --clean-stale is set. Done between phases 2 and 3 so the diff in
	// [FinishedJob.Diff] still sees the per-component dirs that WILL be
	// rewritten by Apply -- which keeps Result.Changed meaningful and
	// reduces the blast radius of a Ctrl-C (we never delete dirs that
	// would have been re-rendered anyway).
	//
	// Skipped in --check-only mode -- the orphan list is computed
	// read-only later via checkOnlyRenderResult.
	if options.CleanStale && !options.CheckOnly {
		if pruneErr := pruneOrphansForComponents(env, options.OutputDir, componentList); pruneErr != nil {
			return nil, pruneErr
		}
	}

	// ── Phase 3 → []FinishedJob ──
	finishedJobs := parallelFinish(env, preparedJobs, mockResultMap)

	// Diff every job (always) and Apply (unless --check-only).
	// Apply errors are recorded into the job's err so Result() reflects
	// them; per-job apply failures do NOT abort the whole render.
	results := finalizeJobs(env, finishedJobs, options.Force, options.CheckOnly)

	sortRenderResults(results)

	if options.CheckOnly {
		return results, checkOnlyRenderResult(env.FS(), options, componentList, results)
	}

	return results, checkRenderErrors(results, options.FailOnError)
}

// buildJobs constructs a [NewJob] per component, in input order.
func buildJobs(comps []components.Component, stagingDirRoot, outputDirRoot string) []NewJob {
	jobs := make([]NewJob, len(comps))
	for idx, comp := range comps {
		jobs[idx] = newRenderJob(comp, stagingDirRoot, outputDirRoot)
	}

	return jobs
}

// finalizeJobs runs Diff (always) and Apply (unless checkOnly) over every
// finished job, then turns each into a [Result]. Returns results in input
// order so the caller can sort them.
func finalizeJobs(env *azldev.Env, jobs []FinishedJob, allowOverwrite, checkOnly bool) []*Result {
	results := make([]*Result, len(jobs))

	for idx, job := range jobs {
		job.Diff(env.FS())

		if !checkOnly {
			if applyErr := job.Apply(env, allowOverwrite); applyErr != nil {
				slog.Error("Failed to apply job",
					"component", job.Result().Component, "error", applyErr)
			}
		}

		results[idx] = job.Result()
	}

	return results
}

// createStagingDir creates the shared staging directory used by every phase.
// Returns a cleanup func that removes the directory; the caller defers it.
//
// Uses the project work dir instead of /tmp to avoid filling up tmpfs on
// large renders. Each component gets a subdirectory named after it,
// enabling a single bind mount for the batch mock invocation.
func createStagingDir(env *azldev.Env) (string, func(), error) {
	if err := env.FS().MkdirAll(env.WorkDir(), fileperms.PublicDir); err != nil {
		return "", nil, fmt.Errorf("creating work directory:\n%w", err)
	}

	stagingDir, err := fileutils.MkdirTemp(env.FS(), env.WorkDir(), "azldev-render-staging-")
	if err != nil {
		return "", nil, fmt.Errorf("creating staging directory:\n%w", err)
	}

	cleanup := func() {
		if removeErr := env.FS().RemoveAll(stagingDir); removeErr != nil {
			slog.Debug("Failed to clean up staging directory", "path", stagingDir, "error", removeErr)
		}
	}

	return stagingDir, cleanup, nil
}

// pruneOrphansForComponents wraps [pruneOrphanRenderedDirs] with the
// component-name extraction and error wrapping the orchestrator wants.
func pruneOrphansForComponents(env *azldev.Env, outputDir string, comps []components.Component) error {
	names := make([]string, len(comps))
	for idx, comp := range comps {
		names[idx] = comp.GetName()
	}

	if err := pruneOrphanRenderedDirs(env.FS(), outputDir, names); err != nil {
		return fmt.Errorf("pruning orphan rendered-spec dirs in %#q:\n%w", outputDir, err)
	}

	return nil
}
