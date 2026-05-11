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

// outputDriftsFromMarker reports whether outputDir's contents diverge from a
// fresh failure write -- i.e., a single RENDER_FAILED file containing the
// canonical marker body. Returns true when the on-disk state would change
// if a real failure write ran. Used by [FinishedJob.Diff] for failure jobs
// to enforce 1:1 parity: a component that would fail must already be
// marked failed on disk, with no extra stale output around it.
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
