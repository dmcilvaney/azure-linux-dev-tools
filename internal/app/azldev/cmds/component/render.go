// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/renderer"
	"github.com/spf13/cobra"
)

func renderOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewRenderCmd())
}

// NewRenderCmd constructs a [cobra.Command] for the "component render" CLI
// subcommand. The actual rendering pipeline lives in the
// [github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/renderer]
// package; this file is purely the CLI ↔ [renderer.Options] glue.
func NewRenderCmd() *cobra.Command {
	var options renderer.Options

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

			return renderer.Render(env, &options)
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
