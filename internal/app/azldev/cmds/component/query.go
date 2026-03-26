// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"fmt"
	"log/slog"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/specs"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/cobra"
)

// Options for querying components from the environment.
type QueryComponentsOptions struct {
	// Standard filter for selecting components.
	ComponentFilter components.ComponentFilter
	// LocalOnly disables the automatic fallback to full source preparation when spec-only
	// parsing fails. When true, the query uses only the local spec directory.
	LocalOnly bool
}

func queryOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewComponentQueryCommand())
}

// Constructs a [cobra.Command] for "component query" CLI subcommand.
func NewComponentQueryCommand() *cobra.Command {
	options := &QueryComponentsOptions{}

	cmd := &cobra.Command{
		Use:   "query",
		Short: "Query info for components in this project",
		Long: `Query detailed information for components by fetching and parsing their spec files.

Unlike 'list', which only shows configuration metadata, 'query' resolves
upstream sources and parses the RPM spec to report version, release,
subpackages, dependencies, and other spec-level details. This makes it
slower than 'list' but more informative.

By default, the query first attempts a fast spec-directory-only parse. If
rpmspec fails (e.g., due to unresolvable macros or missing source files),
it automatically falls back to full source preparation — downloading
archives and applying overlays — before re-querying. Use --local-only to
disable this fallback and fail immediately on spec-only parse errors.`,
		Example: `  # Query a single component
  azldev component query -p curl

  # Query with JSON output
  azldev component query -p bash -q -O json

  # Query without source fallback (spec-directory only)
  azldev component query -p curl --local-only`,
		RunE: azldev.RunFuncWithExtraArgs(func(env *azldev.Env, args []string) (interface{}, error) {
			options.ComponentFilter.ComponentNamePatterns = append(args, options.ComponentFilter.ComponentNamePatterns...)

			return QueryComponents(env, options)
		}),
		ValidArgsFunction: components.GenerateComponentNameCompletions,
	}

	components.AddComponentFilterOptionsToCommand(cmd, &options.ComponentFilter)
	cmd.Flags().BoolVar(&options.LocalOnly, "local-only", false,
		"skip automatic source-preparation fallback; fail immediately if spec-only parsing fails")

	return cmd
}

// componentDetails encapsulates detailed information about a component.
type componentDetails struct {
	specs.ComponentSpecDetails
}

// Queries env for component details, in accordance with options. Returns the found components.
func QueryComponents(
	env *azldev.Env, options *QueryComponentsOptions,
) (results []*componentDetails, err error) {
	var comps *components.ComponentSet

	resolver := components.NewResolver(env)

	comps, err = resolver.FindComponents(&options.ComponentFilter)
	if err != nil {
		return results, fmt.Errorf("failed to resolve components:\n%w", err)
	}

	allDetails := make([]*componentDetails, 0, comps.Len())

	for _, comp := range comps.Components() {
		spec := comp.GetSpec()

		specInfo, parseErr := spec.Parse()
		if parseErr != nil && !options.LocalOnly {
			slog.Warn("Spec-only parse failed; retrying with full source preparation",
				"component", comp.GetName(), "error", parseErr)

			specInfo, parseErr = queryWithSourceFallback(env, comp, spec)
		}

		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse spec for component %q:\n%w", comp.GetName(), parseErr)
		}

		details := &componentDetails{
			ComponentSpecDetails: *specInfo,
		}

		allDetails = append(allDetails, details)
	}

	return allDetails, nil
}

// queryWithSourceFallback prepares full sources for the component (downloading archives and
// applying overlays) and re-attempts spec parsing with the prepared sources directory
// bind-mounted into the build environment. This allows rpmspec to resolve macros and source
// references that depend on files beyond the raw spec directory.
func queryWithSourceFallback(
	env *azldev.Env, comp components.Component, spec specs.ComponentSpec,
) (*specs.ComponentSpecDetails, error) {
	// Resolve the distro for this component to create the source manager.
	distro, err := sourceproviders.ResolveDistro(env, comp)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve distro for component %#q during source fallback:\n%w",
			comp.GetName(), err)
	}

	sourceManager, err := sourceproviders.NewSourceManager(env, distro)
	if err != nil {
		return nil, fmt.Errorf("failed to create source manager during source fallback:\n%w", err)
	}

	preparer, err := sources.NewPreparer(sourceManager, env.FS(), env, env)
	if err != nil {
		return nil, fmt.Errorf("failed to create source preparer during source fallback:\n%w", err)
	}

	// Create a temp directory for the prepared sources.
	sourcesDir, err := fileutils.MkdirTemp(env.FS(), "", "query-sources-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for source fallback:\n%w", err)
	}

	defer func() {
		if removeErr := env.FS().RemoveAll(sourcesDir); removeErr != nil {
			slog.Warn("Failed to clean up source fallback temp dir", "dir", sourcesDir, "error", removeErr)
		}
	}()

	// Prepare sources with overlays applied so rpmspec sees the final state.
	err = preparer.PrepareSources(env, comp, sourcesDir, true /* applyOverlays */)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare sources for component %#q during source fallback:\n%w",
			comp.GetName(), err)
	}

	// Re-attempt parsing with the fully prepared sources directory.
	specInfo, err := spec.ParseFromDir(sourcesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to parse spec from prepared sources for component %#q:\n%w",
			comp.GetName(), err)
	}

	return specInfo, nil
}
