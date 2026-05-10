// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// renderErrorMarkerFile is the name of the marker file written to a component's
// output directory when rendering fails. This makes the failure visible in git diff
// when the issue is later fixed (the marker file disappears, replaced by real output).
const renderErrorMarkerFile = "RENDER_FAILED"

// renderErrorMarkerContent is the static body of the RENDER_FAILED marker file.
// It must match exactly what writeRenderErrorMarker writes; --check-only relies
// on this constant to verify on-disk failure markers are byte-identical to a
// fresh run's output.
const renderErrorMarkerContent = "Rendering failed. See azldev logs for details.\n"

// writeRenderErrorMarker writes a static marker file to the component's output directory
// indicating that rendering failed. The content is intentionally static (no error details)
// so the file is deterministic across runs and safe to check in.
//
// This is always written on failure, even without --force. The --force flag controls
// deletion of existing directories (RemoveAll), not creation of new files. Writing a
// small marker into an existing directory is safe and provides visible git diff feedback.
func writeRenderErrorMarker(fs opctx.FS, componentOutputDir string) {
	if mkdirErr := fileutils.MkdirAll(fs, componentOutputDir); mkdirErr != nil {
		slog.Debug("Failed to create directory for error marker", "path", componentOutputDir, "error", mkdirErr)

		return
	}

	markerPath := filepath.Join(componentOutputDir, renderErrorMarkerFile)

	if writeErr := fileutils.WriteFile(
		fs, markerPath, []byte(renderErrorMarkerContent), fileperms.PublicFile,
	); writeErr != nil {
		slog.Debug("Failed to write render error marker", "path", markerPath, "error", writeErr)
	}
}

// writeFailureMarkers walks the final results slice and writes a RENDER_FAILED
// marker into each errored component's output directory. When allowOverwrite
// is set, any pre-existing content at the path is removed first so the marker
// isn't surrounded by stale render output.
//
// Cancelled components are intentionally skipped — a Ctrl-C is not a render
// failure, just an incomplete run, and silently planting markers under those
// circumstances would lie in git diff.
//
// In --check-only mode, no marker is written. Instead, the existing on-disk
// state for each errored component is verified to be exactly the standard
// failure marker; any deviation flips result.Changed so the caller can fail
// the run. This delivers the 1:1 invariant the user asked for: a component
// that would fail must already be marked as failed on disk, with no extra
// stale output around it.
func writeFailureMarkers(
	fileSystem opctx.FS, results []*Result, allowOverwrite, checkOnly bool,
) {
	for _, result := range results {
		if result == nil || result.Status != renderStatusError {
			continue
		}

		if checkOnly {
			drifted, err := outputDriftsFromMarker(fileSystem, result.OutputDir)
			if err != nil {
				// Surface inspection errors at Warn so a CI failure is
				// debuggable. Treat them as drift -- safer to fail loudly
				// than silently pass.
				slog.Warn("Failed to inspect output dir for failure-marker check; treating as drift",
					"path", result.OutputDir, "error", err)

				result.Changed = true

				continue
			}

			if drifted {
				result.Changed = true
			}

			continue
		}

		if allowOverwrite {
			if removeErr := fileSystem.RemoveAll(result.OutputDir); removeErr != nil {
				slog.Debug("Failed to clean output before writing error marker",
					"path", result.OutputDir, "error", removeErr)
			}
		}

		writeRenderErrorMarker(fileSystem, result.OutputDir)
	}
}

// outputDriftsFromMarker reports whether outputDir's contents diverge from a
// fresh failure write -- i.e., a single RENDER_FAILED file containing the
// canonical marker body. Returns true when the on-disk state would change
// if a real failure write ran. Used by --check-only to enforce 1:1 parity:
// a component that would fail must already be marked failed on disk, with
// no extra stale output around it.
func outputDriftsFromMarker(fileSystem opctx.FS, outputDir string) (bool, error) {
	exists, err := fileutils.DirExists(fileSystem, outputDir)
	if err != nil {
		return false, fmt.Errorf("checking output dir %#q:\n%w", outputDir, err)
	}

	if !exists {
		return true, nil
	}

	entries, err := fileutils.ReadDir(fileSystem, outputDir)
	if err != nil {
		return false, fmt.Errorf("reading output dir %#q:\n%w", outputDir, err)
	}

	if len(entries) != 1 || entries[0].Name() != renderErrorMarkerFile {
		return true, nil
	}

	content, err := fileutils.ReadFile(fileSystem, filepath.Join(outputDir, renderErrorMarkerFile))
	if err != nil {
		return false, fmt.Errorf("reading marker %#q:\n%w", outputDir, err)
	}

	return string(content) != renderErrorMarkerContent, nil
}
