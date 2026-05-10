// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
)

// Result holds the result of rendering a single component.
type Result struct {
	Component string `json:"component"       table:"Component"`
	OutputDir string `json:"outputDir"       table:"Output"`
	Status    string `json:"status"          table:"Status"`
	Error     string `json:"error,omitempty" table:"Error,omitempty"`
	Changed   bool   `json:"changed"         table:"Changed"`
}

// Render status constants.
const (
	renderStatusOK        = "ok"
	renderStatusError     = "error"
	renderStatusCancelled = "cancelled"
)

// checkOnlyRenderResult inspects results from a --check-only run and returns
// a non-nil error when any component changed or any orphan rendered-spec
// directory was detected. Orphan detection runs only with -a + --clean-stale
// (the only configuration where a normal run would actually remove orphans).
// The error message names every changed component and orphan so CI logs are
// useful at a glance.
func checkOnlyRenderResult(
	fileSystem opctx.FS,
	options *Options,
	resolvedComps []components.Component,
	results []*Result,
) error {
	var changed []string

	for _, result := range results {
		if result != nil && result.Changed {
			changed = append(changed, result.Component)
		}
	}

	var orphans []string

	if options.ComponentFilter.IncludeAllComponents && options.CleanStale {
		names := make([]string, len(resolvedComps))
		for idx, comp := range resolvedComps {
			names[idx] = comp.GetName()
		}

		found, err := findOrphanRenderedDirs(fileSystem, options.OutputDir, names)
		if err != nil {
			return fmt.Errorf("checking for orphan rendered-spec dirs:\n%w", err)
		}

		orphans = found
	}

	if len(changed) == 0 && len(orphans) == 0 {
		return nil
	}

	parts := make([]string, 0)
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d component(s) would change: %s",
			len(changed), strings.Join(changed, ", ")))
	}

	if len(orphans) > 0 {
		parts = append(parts, fmt.Sprintf("%d orphan rendered-spec dir(s): %s",
			len(orphans), strings.Join(orphans, ", ")))
	}

	return fmt.Errorf("rendered output is stale; %s. Run 'azldev component render -a' to refresh",
		strings.Join(parts, "; "))
}

// sortRenderResults sorts render results alphabetically by component name,
// with nil entries sorted to the end.
func sortRenderResults(results []*Result) {
	slices.SortFunc(results, func(left, right *Result) int {
		switch {
		case left == nil && right == nil:
			return 0
		case left == nil:
			return 1 // nils sort to end
		case right == nil:
			return -1
		default:
			return strings.Compare(left.Component, right.Component)
		}
	})
}

// checkRenderErrors counts error and cancelled results and returns an error if FailOnError is set.
func checkRenderErrors(results []*Result, failOnError bool) error {
	var errCount, cancelledCount int

	for _, result := range results {
		if result == nil {
			continue
		}

		switch result.Status {
		case renderStatusError:
			errCount++
		case renderStatusCancelled:
			cancelledCount++
		}
	}

	failCount := errCount + cancelledCount

	if failCount > 0 {
		slog.Error("Some components failed to render",
			"errorCount", errCount, "cancelledCount", cancelledCount)

		if failOnError {
			return fmt.Errorf("%d component(s) failed to render", failCount)
		}
	}

	// When FailOnError is not set, intentionally return nil error even when
	// some components fail. Returning an error would suppress the results
	// table (runFuncInternal skips reportResults on error), hiding the status
	// of all ~7k components. Individual failures are visible in the table's
	// Status/Error columns and via RENDER_FAILED marker files.
	return nil
}
