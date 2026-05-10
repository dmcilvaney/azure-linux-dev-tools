// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// pruneOrphanRenderedDirs removes per-component rendered-spec directories
// under outputDir that don't correspond to any component in resolvedComps.
// Per-component dirs that ARE in resolvedComps are left alone -- phase 3 will
// overwrite them via copyRenderedOutput, and leaving the prior content here
// lets the unconditional diff in finishComponentRender produce a meaningful
// result.Changed value for the user-visible table.
//
// Top-level non-letter entries (e.g. a hand-placed README.md at the root of
// SPECS/) are intentionally NOT removed. The previous implementation wiped
// them too, but in practice the only callers want orphan cleanup, not a
// blanket sweep.
func pruneOrphanRenderedDirs(
	fileSystem opctx.FS, outputDir string, componentNames []string,
) error {
	orphans, err := findOrphanRenderedDirs(fileSystem, outputDir, componentNames)
	if err != nil {
		return err
	}

	for _, rel := range orphans {
		fullPath := filepath.Join(outputDir, rel)
		if removeErr := fileSystem.RemoveAll(fullPath); removeErr != nil {
			return fmt.Errorf("removing orphan rendered-spec dir %#q:\n%w", fullPath, removeErr)
		}

		slog.Info("Removed orphan rendered-spec dir", "path", fullPath)
	}

	return nil
}

// findOrphanRenderedDirs returns the names of rendered-spec directories under
// outputDir that don't correspond to any resolved component (or its alias).
// Names are returned as "<letter>/<name>" relative paths and sorted.
//
// Only meaningful with -a (we know the full component set) and --clean-stale
// (the only configuration where a normal run would actually remove orphans).
// Top-level non-letter entries are intentionally NOT flagged -- that matches
// the existing wipe semantics where users may store unrelated siblings in a
// custom output dir; flagging them here would surprise CI gates.
func findOrphanRenderedDirs(
	fileSystem opctx.FS, outputDir string, componentNames []string,
) ([]string, error) {
	exists, err := fileutils.DirExists(fileSystem, outputDir)
	if err != nil {
		return nil, fmt.Errorf("checking output dir %#q:\n%w", outputDir, err)
	}

	if !exists {
		return nil, nil
	}

	expectedNames := make(map[string]struct{}, len(componentNames)*2) //nolint:mnd // name + optional alias
	for _, name := range componentNames {
		expectedNames[name] = struct{}{}

		if alias := components.RenderedSpecDirAliasName(name); alias != "" {
			expectedNames[alias] = struct{}{}
		}
	}

	letterDirs, err := fileutils.ReadDir(fileSystem, outputDir)
	if err != nil {
		return nil, fmt.Errorf("reading output dir %#q:\n%w", outputDir, err)
	}

	var orphans []string

	for _, letterEntry := range letterDirs {
		if !letterEntry.IsDir() {
			continue
		}

		// Only descend into single-character prefix dirs (a/, c/, ...) --
		// matches the layout written by [components.RenderedSpecDir]. A
		// hand-placed sibling like 'tooling/' or 'overlays/' is left
		// alone; treating its children as orphans would silently delete
		// unrelated content on the next --clean-stale run.
		if len(letterEntry.Name()) != 1 {
			continue
		}

		letterPath := filepath.Join(outputDir, letterEntry.Name())

		children, readErr := fileutils.ReadDir(fileSystem, letterPath)
		if readErr != nil {
			return nil, fmt.Errorf("reading letter dir %#q:\n%w", letterPath, readErr)
		}

		for _, child := range children {
			// Component output is always a directory. Stray files (e.g. an
			// editor's swap file or a hand-placed .gitkeep) are not orphan
			// rendered-spec dirs and must not be flagged for removal.
			if !child.IsDir() {
				continue
			}

			if _, ok := expectedNames[child.Name()]; !ok {
				orphans = append(orphans, filepath.Join(letterEntry.Name(), child.Name()))
			}
		}
	}

	sort.Strings(orphans)

	return orphans, nil
}
