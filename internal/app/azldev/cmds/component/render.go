// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package component implements the `azldev component` subcommands.
//
// The render pipeline is structured across several files for readability:
//
//   - render.go (this file)       — cobra wiring, RenderOptions, the top-level
//     RenderComponents pipeline, and the two
//     intermediate types it passes between phases.
//   - render_options.go           — output-dir resolution, validation helpers,
//     mock-processor construction.
//   - render_result.go            — RenderResult type, status consts, sort,
//     error/check-only summarizers.
//   - render_marker.go            — RENDER_FAILED marker contract (write, read,
//     verify) used by failed/cancelled components.
//   - render_orphans.go           — per-component dir orphan detection and
//     pruning for --clean-stale runs.
//   - render_phase_prepare.go     — phase 1: parallel source preparation.
//   - render_phase_mock.go        — phase 2: batch mock invocation.
//   - render_phase_finish.go      — phase 3: filter, .git strip, diff, copy.
package component

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/cobra"
)

// RenderOptions holds the options for the render command.
type RenderOptions struct {
	ComponentFilter   components.ComponentFilter
	OutputDir         string
	OutputDirExplicit bool // True when --output-dir was explicitly passed on the CLI.
	FailOnError       bool
	Force             bool
	CleanStale        bool
	CheckOnly         bool
}

func renderOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewRenderCmd())
}

// NewRenderCmd constructs a [cobra.Command] for the "component render" CLI subcommand.
func NewRenderCmd() *cobra.Command {
	var options RenderOptions

	var cmd *cobra.Command

	cmd = &cobra.Command{
		Use:   "render",
		Short: "Render post-overlay specs and sidecar files to a checked-in directory",
		Long: `Render the final spec and sidecar files for components after applying all
configured overlays. The output is written to a directory as generated artifacts
intended for check-in.

The output directory is set via rendered-specs-dir in the project config, or
via --output-dir on the command line. If neither is set, an error is returned.
Within the output directory, components are organized into letter-prefixed
subdirectories based on the first character of their name (e.g., specs/c/curl,
specs/v/vim).

Unlike prepare-sources, render skips downloading source tarballs from the
lookaside cache — only spec files, patches, scripts, and other git-tracked
sidecar files are included. Multiple components can be rendered at once.

When rendering all components (-a), the --clean-stale flag prunes orphan
rendered-spec directories (per-component dirs that no longer correspond to
any component in the project config). Per-component dirs that ARE in config
are overwritten in place by the render itself; this means each render's
result table accurately reflects which components actually changed on disk.
Top-level non-component siblings (e.g. a hand-placed README.md) are
preserved. When using a custom output directory (--output-dir), --force is
required alongside --clean-stale as a safety measure. This flag is only
valid with -a.`,
		Example: `  # Render all components (output dir from config)
  azldev component render -a

  # Render a single component
  azldev component render -p curl

  # Render to a custom directory, allowing removal of existing rendered component directories
  azldev component render -a -o rendered/ --force

  # Render all and remove stale directories
  azldev component render -a --clean-stale`,
		RunE: azldev.RunFuncWithExtraArgs(func(env *azldev.Env, args []string) (interface{}, error) {
			options.ComponentFilter.ComponentNamePatterns = append(args, options.ComponentFilter.ComponentNamePatterns...)
			options.OutputDirExplicit = cmd.Flags().Changed("output-dir")

			return RenderComponents(env, &options)
		}),
		ValidArgsFunction: components.GenerateComponentNameCompletions,
		Annotations: map[string]string{
			azldev.CommandAnnotationRootOK: "true",
		},
	}

	components.AddComponentFilterOptionsToCommand(cmd, &options.ComponentFilter)

	cmd.Flags().StringVarP(&options.OutputDir, "output-dir", "o", "",
		"output directory for rendered specs (overrides rendered-specs-dir from config)")
	_ = cmd.MarkFlagDirname("output-dir")

	cmd.Flags().BoolVar(&options.FailOnError, "fail-on-error", false,
		"exit with error if any component fails to render (useful for CI)")

	cmd.Flags().BoolVarP(&options.Force, "force", "f", false,
		"allow overwriting existing rendered component directories")

	cmd.Flags().BoolVar(&options.CleanStale, "clean-stale", false,
		"prune rendered-spec directories that no longer correspond to a configured "+
			"component (only with -a; requires -f with -o). Top-level non-component "+
			"siblings are preserved.")

	cmd.Flags().BoolVar(&options.CheckOnly, "check-only", false,
		"render to a staging area and compare against the existing on-disk output "+
			"without modifying the output directory. Exits 0 when nothing would change "+
			"and 1 when any component would drift. With -a + --clean-stale, also fails "+
			"on orphan rendered-spec directories. Intended for CI gates.")

	// --check-only is a read-only diff against on-disk state; --fail-on-error
	// is the loud-failure-per-run knob. Combining them is semantically
	// muddled (CI would fail on stale failures even when on-disk markers
	// already record them) and forcing a choice keeps the contract crisp.
	cmd.MarkFlagsMutuallyExclusive("fail-on-error", "check-only")

	return cmd
}

// preparedComponent holds the intermediate state after source preparation,
// before mock processing. Lives here (not in render_phase_prepare.go) because
// it is the contract between phases 1, 2, and 3.
type preparedComponent struct {
	index         int
	comp          components.Component
	specFilename  string // e.g., "curl.spec"
	compOutputDir string // validated output path computed in phase 1
}

// prepResult pairs a prepared component (on success) or a render result (on error).
// Returned from each phase-1 worker to its caller.
type prepResult struct {
	prepared *preparedComponent
	result   *RenderResult // non-nil on error
}

// RenderComponents renders the post-overlay spec and sidecar files for each
// selected component into the output directory. Processing is done in three phases:
//  1. Parallel source preparation (clone, overlay, synthetic git)
//  2. Batch mock processing (rpmautospec + spectool in a single chroot call)
//  3. Parallel finishing (filter files, remove .git, copy output)
func RenderComponents(env *azldev.Env, options *RenderOptions) ([]*RenderResult, error) {
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
	results := make([]*RenderResult, len(componentList))

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
