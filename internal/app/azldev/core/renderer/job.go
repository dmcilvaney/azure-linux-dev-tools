// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// Job is the minimum view of a render job: callable at every lifecycle stage.
// The orchestrator uses this composite to build the final Result table
// regardless of how far a job got through the pipeline.
type Job interface {
	// Result returns the user-visible result row for this job. Safe to call
	// at any stage; reflects whatever the job has accumulated so far.
	Result() *Result
}

// NewJob is a freshly constructed job, before any phase has run. Its only
// productive moves are forward — either Prepare (run phase 1) or
// MarkCancelled (parmap-cancelled before the worker started).
type NewJob interface {
	Job

	// Prepare runs phase 1: clone + overlay + synthetic git, populating the
	// component's staging directory. Returns the same job advanced to the
	// PreparedJob stage. On failure the returned job carries the error
	// internally and subsequent methods short-circuit; the type system
	// still hands you a PreparedJob so the pipeline shape stays uniform.
	Prepare(env *azldev.Env) PreparedJob

	// MarkCancelled records context.Canceled as the job's error and
	// advances it to PreparedJob without running phase 1. Used when parmap
	// reports that the worker never got a slot before the parent ctx ended.
	MarkCancelled() PreparedJob
}

// PreparedJob is a job that has been through (or has been advanced past)
// phase 1. Phase 2 (batch mock) consumes a slice of these.
type PreparedJob interface {
	Job

	// SpecFilename returns the spec basename that phase 2 needs to invoke
	// mock for this component. Returns "" for jobs that failed phase 1 or
	// were cancelled; the orchestrator skips those when assembling the
	// mock batch.
	SpecFilename() string

	// ComponentName returns the component's name. Used by phase 2 to key
	// the batch mock input by name. Defined here rather than on the Job
	// interface because Result()'s Component field is only safe to read
	// after the job has its identity fields populated, which both
	// PreparedJob and FinishedJob guarantee.
	ComponentName() string

	// ApplyMockResult runs phase 3 for this job: filter unreferenced
	// files using the spectool output, then strip .git. Returns the same
	// job advanced to FinishedJob. A nil mockResult (e.g. the whole batch
	// mock call failed) is treated as a failure.
	ApplyMockResult(env *azldev.Env, mockResult *sources.ComponentMockResult) FinishedJob
}

// FinishedJob is the terminal stage: phases 1 through 3 are done (or were
// skipped because the job failed earlier). The orchestrator can now diff
// the desired output against disk and optionally apply.
type FinishedJob interface {
	Job

	// Diff compares the desired output against what is on disk and stores
	// the result internally so Result.Changed reflects it. Always safe to
	// call. Reports drift for failed jobs by comparing on-disk state
	// against the canonical RENDER_FAILED marker.
	Diff(fs opctx.FS)

	// Apply writes the job's desired state to disk:
	//   - cancelled (j.err == context.Canceled) → no-op
	//   - failed    (j.err != nil)              → write RENDER_FAILED marker
	//   - success   (j.err == nil)              → copy staging → output dir
	// allowOverwrite mirrors the --force flag (controls whether existing
	// output dirs may be replaced).
	//
	// Errors are recorded internally (subsequent Result() reports them) and
	// also returned so the caller can log; a per-job apply failure does not
	// abort the rest of the run.
	Apply(env *azldev.Env, allowOverwrite bool) error
}

// renderJob is the single concrete type backing all three view interfaces.
// Each phase method returns the receiver typed as the next view, advancing
// the job through its lifecycle without copying.
//
// Fields are mutated as the job progresses; this is safe because every
// renderJob is processed by exactly one parmap worker at a time (parmap
// guarantees per-item serialization).
type renderJob struct {
	// Set at construction.
	comp           components.Component
	stagingDirRoot string // shared root; compStagingDir lives under this
	compStagingDir string // stagingDirRoot/<componentName>
	compOutputDir  string // <outputDir>/<letter>/<componentName>

	// Set by Prepare on success.
	specFilename string

	// err encodes the job's terminal state. See the package-level comment
	// on this field's contract:
	//   - nil                              : in-progress or success
	//   - errors.Is(err, context.Canceled) : cancelled before phase 1 ran;
	//                                        Apply is a no-op
	//   - any other non-nil                : phase failure; Apply writes
	//                                        the RENDER_FAILED marker
	err error

	// Set by Diff. Read by Result.
	changed bool
}

// newRenderJob constructs a job. The output directory is computed eagerly so
// even a job that fails before Prepare can produce a useful Result.OutputDir.
//
// A name-validation failure becomes the job's terminal err; subsequent
// phase methods short-circuit and Apply writes a RENDER_FAILED marker at
// the placeholder "(invalid)" path. (In practice that path is never
// actually written to because validation also produces an unwritable
// path; the marker write fails silently. The user-visible signal is the
// error message in Result.Error.)
func newRenderJob(comp components.Component, stagingDirRoot, outputDirRoot string) NewJob {
	componentName := comp.GetName()

	compOutputDir, nameErr := components.RenderedSpecDir(outputDirRoot, componentName)
	if nameErr != nil {
		return &renderJob{
			comp:           comp,
			stagingDirRoot: stagingDirRoot,
			compOutputDir:  "(invalid)",
			err:            nameErr,
		}
	}

	return &renderJob{
		comp:           comp,
		stagingDirRoot: stagingDirRoot,
		compStagingDir: filepath.Join(stagingDirRoot, componentName),
		compOutputDir:  compOutputDir,
	}
}

// Result implements [Job]. Status/Error are derived from j.err; Changed is
// whatever Diff stored.
func (j *renderJob) Result() *Result {
	return &Result{
		Component: j.comp.GetName(),
		OutputDir: j.compOutputDir,
		Status:    j.statusString(),
		Error:     j.errorString(),
		Changed:   j.changed,
	}
}

// statusString maps j.err to the Result.Status string.
func (j *renderJob) statusString() string {
	switch {
	case j.err == nil:
		return renderStatusOK
	case errors.Is(j.err, context.Canceled):
		return renderStatusCancelled
	default:
		return renderStatusError
	}
}

// errorString maps j.err to the Result.Error string. Cancellation is
// surfaced as a fixed message so the table reads consistently regardless
// of where in the pipeline the ctx ended.
func (j *renderJob) errorString() string {
	switch {
	case j.err == nil:
		return ""
	case errors.Is(j.err, context.Canceled):
		return "context cancelled"
	default:
		return j.err.Error()
	}
}

// ComponentName implements [PreparedJob].
func (j *renderJob) ComponentName() string { return j.comp.GetName() }

// SpecFilename implements [PreparedJob]. Returns "" for failed/cancelled jobs.
func (j *renderJob) SpecFilename() string { return j.specFilename }

// Prepare implements [NewJob]. Runs phase 1 unless the job already failed
// at construction (name validation).
func (j *renderJob) Prepare(env *azldev.Env) PreparedJob {
	if j.err != nil {
		return j
	}

	specFilename, err := prepareComponentSources(env, j.comp, j.stagingDirRoot)
	if err != nil {
		slog.Error("Failed to prepare component sources",
			"component", j.comp.GetName(), "error", err)

		j.err = err

		return j
	}

	j.specFilename = specFilename

	return j
}

// MarkCancelled implements [NewJob]. Records context.Canceled as the
// terminal err so Apply will no-op.
func (j *renderJob) MarkCancelled() PreparedJob {
	if j.err == nil {
		j.err = context.Canceled
	}

	return j
}

// ApplyMockResult implements [PreparedJob]. Runs phase 3 (filter + strip
// .git) unless the job already failed/cancelled, in which case it's a
// passthrough.
func (j *renderJob) ApplyMockResult(env *azldev.Env, mockResult *sources.ComponentMockResult) FinishedJob {
	if j.err != nil {
		return j
	}

	// Bail out if ctx is already done so we don't keep mutating staging
	// after a Ctrl-C while the worker pool is draining.
	if env.Err() != nil {
		j.err = context.Canceled

		return j
	}

	if err := applyMockResultToStaging(env, j.comp, j.compStagingDir, j.specFilename, mockResult); err != nil {
		slog.Error("Failed to finish rendering component",
			"component", j.comp.GetName(), "error", err)

		j.err = err
	}

	return j
}

// Diff implements [FinishedJob]. Errors are logged as warnings and treated
// as drift -- safer to flag a "would change" than to silently pass when we
// couldn't read the disk state.
func (j *renderJob) Diff(fs opctx.FS) {
	changed, err := j.computeDiff(fs)
	if err != nil {
		slog.Warn("Failed to diff job against on-disk state; treating as drift",
			"component", j.comp.GetName(), "error", err)

		j.changed = true

		return
	}

	j.changed = changed
}

// computeDiff returns whether [Apply] would change the disk state. Split
// out from Diff so the error-handling lives in one place.
func (j *renderJob) computeDiff(fs opctx.FS) (bool, error) {
	switch {
	case errors.Is(j.err, context.Canceled):
		// Cancelled jobs never touch disk, so they never drift.
		return false, nil

	case j.err != nil:
		// Failure jobs want exactly the RENDER_FAILED marker on disk.
		return outputDriftsFromMarker(fs, j.compOutputDir)

	default:
		// Success jobs want the staging tree at compOutputDir.
		return diffRenderedOutput(fs, j.compStagingDir, j.compOutputDir)
	}
}

// Apply implements [FinishedJob]. Dispatches on j.err to the right write.
//
// Apply errors update j.err so Result() reflects them. The error is also
// returned for caller logging convenience; the orchestrator does not abort
// on a per-job apply failure.
func (j *renderJob) Apply(env *azldev.Env, allowOverwrite bool) error {
	switch {
	case errors.Is(j.err, context.Canceled):
		// No-op: a cancelled job leaves whatever was on disk alone.
		return nil

	case j.err != nil:
		return j.applyFailureMarker(env, allowOverwrite)

	default:
		return j.applyContent(env, allowOverwrite)
	}
}

// applyContent copies the staging tree to the output dir, then writes the
// best-effort alias symlink. The slog.Info "Rendered component" message
// lives here so it only fires for actually-rendered (not cancelled, not
// failed, not check-only) components.
func (j *renderJob) applyContent(env *azldev.Env, allowOverwrite bool) error {
	if err := copyRenderedOutput(env, j.compStagingDir, j.compOutputDir, allowOverwrite); err != nil {
		j.err = err

		return err
	}

	// Best-effort: create a sibling symlink at the URL-encoded component name
	// to bridge a path-encoding mismatch. We percent-encode component names
	// like 'libxml++' into 'libxml%2B%2B' when building the SCM URL fragment
	// passed to the build host (koji), but the build system then uses that
	// fragment as a filesystem path without decoding it. The symlink lets the
	// build host find the component under either form.
	//
	//nolint:godox // tracked by TODO(koji-fragment-decode) tag.
	// TODO(koji-fragment-decode): remove once the build system decodes fragments.
	if aliasErr := writeAliasSymlink(env.FS(), j.compOutputDir, j.comp.GetName()); aliasErr != nil {
		slog.Warn("Failed to create rendered-spec alias symlink; downstream build steps"+
			" that consume the percent-encoded path may fail to locate this component",
			"component", j.comp.GetName(), "error", aliasErr)
	}

	slog.Info("Rendered component", "component", j.comp.GetName(), "output", j.compOutputDir)

	return nil
}

// applyFailureMarker writes the RENDER_FAILED marker to the output dir.
// When allowOverwrite is set, any pre-existing content is removed first so
// the marker isn't surrounded by stale render output.
//
// Marker write failures are not propagated as errors: the marker is a UX
// hint (visible in git diff), not the failure of record (j.err already
// carries the real failure).
func (j *renderJob) applyFailureMarker(env *azldev.Env, allowOverwrite bool) error {
	if allowOverwrite {
		if removeErr := env.FS().RemoveAll(j.compOutputDir); removeErr != nil {
			slog.Debug("Failed to clean output before writing error marker",
				"path", j.compOutputDir, "error", removeErr)
		}
	}

	writeRenderErrorMarker(env.FS(), j.compOutputDir)

	return nil
}

// Compile-time assertions that *renderJob satisfies every interface view.
var (
	_ NewJob      = (*renderJob)(nil)
	_ PreparedJob = (*renderJob)(nil)
	_ FinishedJob = (*renderJob)(nil)
)

// prepareComponentSources resolves the distro, creates a source manager,
// and prepares sources for a single component. Returns the spec filename
// (so the caller doesn't need to filepath.Base it) or an error.
//
// Defined here rather than in render_phase_prepare.go because it's the
// concrete work the [renderJob.Prepare] method delegates to; keeping them
// in the same file lets a reader trace a single component's prepare path
// without a file hop.
func prepareComponentSources(
	env *azldev.Env,
	comp components.Component,
	stagingDirRoot string,
) (string, error) {
	componentName := comp.GetName()

	event := env.StartEvent("Preparing component sources", "component", componentName)
	defer event.End()

	distro, err := sourceproviders.ResolveDistro(env, comp)
	if err != nil {
		return "", fmt.Errorf("resolving distro for %#q:\n%w", componentName, err)
	}

	sourceManager, err := sourceproviders.NewSourceManager(env, distro)
	if err != nil {
		return "", fmt.Errorf("creating source manager for %#q:\n%w", componentName, err)
	}

	componentDir := filepath.Join(stagingDirRoot, componentName)
	if mkdirErr := fileutils.MkdirAll(env.FS(), componentDir); mkdirErr != nil {
		return "", fmt.Errorf("creating component staging directory:\n%w", mkdirErr)
	}

	// Prepare sources with overlays, skipping lookaside downloads.
	// WithGitRepo preserves upstream .git and creates synthetic history so
	// rpmautospec can expand %autorelease and %autochangelog correctly.
	// WithSkipLookaside avoids expensive tarball downloads -- only spec +
	// sidecar files are needed for rendering.
	preparerOpts := []sources.PreparerOption{
		sources.WithGitRepo(env, env.LockReader()),
		sources.WithSkipLookaside(),
	}

	preparer, err := sources.NewPreparer(sourceManager, env.FS(), env, env, preparerOpts...)
	if err != nil {
		return "", fmt.Errorf("creating source preparer for %#q:\n%w", componentName, err)
	}

	if prepErr := preparer.PrepareSources(env, comp, componentDir, true /*applyOverlays*/); prepErr != nil {
		return "", fmt.Errorf("preparing sources for %#q:\n%w", componentName, prepErr)
	}

	specPath, specErr := findSpecFile(env.FS(), componentDir, componentName)
	if specErr != nil {
		return "", fmt.Errorf("finding spec file for %#q:\n%w", componentName, specErr)
	}

	return filepath.Base(specPath), nil
}

// applyMockResultToStaging filters unreferenced files using the spectool
// output and removes the .git directory from the component's staging dir.
// On return the directory contents reflect the final rendered tree.
//
// Defined here rather than in render_phase_finish.go for the same reason
// as prepareComponentSources: a reader tracing what
// [renderJob.ApplyMockResult] does should see the work in one file.
func applyMockResultToStaging(
	env *azldev.Env,
	comp components.Component,
	componentDir, specFilename string,
	mockResult *sources.ComponentMockResult,
) error {
	componentName := comp.GetName()

	if mockResult == nil {
		return fmt.Errorf(
			"no mock result for %#q (batch mock processing failed; see earlier errors)", componentName)
	}

	if mockResult.Error != nil {
		return fmt.Errorf("mock processing failed for %#q:\n%w", componentName, mockResult.Error)
	}

	specPath := filepath.Join(componentDir, specFilename)

	// Filter files using spectool result from batch mock. Skip when:
	//   1. The component config explicitly opts out via 'skip-file-filter'.
	//   2. spectool output contains unexpanded RPM macros (%{...}), since
	//      the reported filenames don't match the real files on disk.
	switch {
	case comp.GetConfig().Render.SkipFileFilter:
		slog.Info("Skipping file filter ('skip-file-filter' is set)", "component", componentName)
	default:
		if macro := findUnexpandedMacro(mockResult.SpecFiles); macro != "" {
			slog.Info("Skipping file filter (spectool output contains unexpanded macros)",
				"component", componentName, "example", macro)
		} else if filterErr := removeUnreferencedFiles(
			env.FS(), componentDir, specPath, mockResult.SpecFiles, componentName,
		); filterErr != nil {
			return fmt.Errorf("filtering unreferenced files for %#q:\n%w", componentName, filterErr)
		}
	}

	// Remove .git directory -- must not appear in rendered output.
	// rpmautospec already read it during the batch mock phase.
	gitDir := filepath.Join(componentDir, ".git")
	if removeErr := env.FS().RemoveAll(gitDir); removeErr != nil {
		slog.Debug("Failed to remove .git directory", "path", gitDir, "error", removeErr)
	}

	return nil
}
