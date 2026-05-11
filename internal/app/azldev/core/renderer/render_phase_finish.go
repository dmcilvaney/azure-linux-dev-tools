// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/dirdiff"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
	"github.com/spf13/afero"
)

// parallelFinish runs phase 3 (apply mock result: filter + strip .git) over
// every prepared job concurrently, bounded by [azldev.Env.IOBoundConcurrency].
//
// Inputs are [PreparedJob]s; outputs are the same jobs advanced to
// [FinishedJob]. Failures and cancellations short-circuit inside the job
// itself, so every entry comes back as a uniformly-typed FinishedJob.
//
// No disk writes to the output dir happen here -- that is
// [FinishedJob.Apply]'s job, and the orchestrator decides whether to call
// it (--check-only skips).
func parallelFinish(
	env *azldev.Env,
	prepared []PreparedJob,
	mockResultMap map[string]*sources.ComponentMockResult,
) []FinishedJob {
	progressEvent := env.StartEvent("Finishing rendered output", "count", len(prepared))
	defer progressEvent.End()

	workerEnv, cancel := env.WithCancel()
	defer cancel()

	total := int64(len(prepared))

	parmapResults := parmap.Map(
		workerEnv,
		env.IOBoundConcurrency(),
		prepared,
		func(done, _ int) { progressEvent.SetProgress(int64(done), total) },
		func(_ context.Context, j PreparedJob) FinishedJob {
			return j.ApplyMockResult(workerEnv, mockResultMap[j.ComponentName()])
		},
	)

	finishedJobs := make([]FinishedJob, len(prepared))

	for idx, result := range parmapResults {
		if result.Cancelled {
			// Worker never started — ctx ended before parmap reached it.
			// Re-run ApplyMockResult with a nil mock; the job's internal
			// short-circuit (env.Err() check) records ctx.Canceled.
			finishedJobs[idx] = prepared[idx].ApplyMockResult(workerEnv, nil)
		} else {
			finishedJobs[idx] = result.Value
		}
	}

	return finishedJobs
}

// findUnexpandedMacro returns the first filename from specFiles that contains
// an unexpanded RPM macro (i.e., a literal "%{...}" sequence), or "" if all
// macros were resolved. When spectool cannot resolve a macro, it emits the raw
// macro text as part of the filename (e.g., "57-%{fontpkgname1}.xml"), which
// won't match any real file on disk.
func findUnexpandedMacro(specFiles []string) string {
	for _, f := range specFiles {
		if strings.Contains(f, "%{") {
			return f
		}
	}

	return ""
}

// removeUnreferencedFiles removes files from the directory that aren't in the keep-list.
// The keep-list is built from the spec file, the "sources" directory, and all
// source/patch filenames provided. For paths with subdirectories (e.g., "patches/fix.patch"),
// the top-level directory ("patches") is kept.
func removeUnreferencedFiles(fs opctx.FS, tempDir, specPath string, specFiles []string, componentName string) error {
	keepSet := make(map[string]bool, len(specFiles))
	keepSet[filepath.Base(specPath)] = true
	keepSet["sources"] = true // lookaside hashes/signatures; always preserved

	for _, f := range specFiles {
		// Extract the first path component so subdirectory entries are preserved.
		topLevel := strings.SplitN(f, string(filepath.Separator), 2)[0] //nolint:mnd // split into at most 2 parts
		keepSet[topLevel] = true
	}

	entries, readErr := fileutils.ReadDir(fs, tempDir)
	if readErr != nil {
		return fmt.Errorf("reading temp directory %#q:\n%w", tempDir, readErr)
	}

	for _, entry := range entries {
		if keepSet[entry.Name()] {
			continue
		}

		removePath := filepath.Join(tempDir, entry.Name())

		slog.Debug("Filtering out unreferenced entry",
			"component", componentName,
			"file", entry.Name(),
		)

		if removeErr := fs.RemoveAll(removePath); removeErr != nil {
			return fmt.Errorf("failed to remove filtered entry %#q for component %#q:\n%w",
				entry.Name(), componentName, removeErr)
		}
	}

	return nil
}

// diffRenderedOutput compares the rendered staging tree (expectedDir) against
// the existing on-disk output (actualDir) and returns true when they differ.
// A missing actualDir always counts as drift. Symlinks are compared by target
// (filesystems without symlink support skip that check; matches production
// render behavior).
func diffRenderedOutput(fileSystem opctx.FS, expectedDir, actualDir string) (bool, error) {
	actualExists, err := fileutils.DirExists(fileSystem, actualDir)
	if err != nil {
		return false, fmt.Errorf("checking actual output dir %#q:\n%w", actualDir, err)
	}

	if !actualExists {
		// First-time render -- every file in expectedDir is drift.
		return true, nil
	}

	result, err := dirdiff.DiffDirs(fileSystem, actualDir, expectedDir)
	if err != nil {
		return false, fmt.Errorf("diffing %#q vs %#q:\n%w", actualDir, expectedDir, err)
	}

	return len(result.Files) > 0, nil
}

// copyRenderedOutput copies the rendered files from tempDir to the component's output directory.
// For managed output (inside project root), existing output is removed before copying.
// For external output, existing directories cause an error.
func copyRenderedOutput(env *azldev.Env, tempDir, componentOutputDir string, allowOverwrite bool) error {
	exists, existsErr := fileutils.DirExists(env.FS(), componentOutputDir)
	if existsErr != nil {
		return fmt.Errorf("checking output directory %#q:\n%w", componentOutputDir, existsErr)
	}

	if exists {
		if !allowOverwrite {
			return fmt.Errorf(
				"output directory %#q already exists; use --force to overwrite",
				componentOutputDir)
		}

		if removeErr := env.FS().RemoveAll(componentOutputDir); removeErr != nil {
			return fmt.Errorf("cleaning output directory %#q:\n%w", componentOutputDir, removeErr)
		}
	}

	if mkdirErr := fileutils.MkdirAll(env.FS(), componentOutputDir); mkdirErr != nil {
		return fmt.Errorf("creating output directory %#q:\n%w", componentOutputDir, mkdirErr)
	}

	copyOptions := fileutils.CopyDirOptions{
		CopyFileOptions: fileutils.CopyFileOptions{
			PreserveFileMode: true,
		},
	}

	if copyErr := fileutils.CopyDirRecursive(env, env.FS(), tempDir, componentOutputDir, copyOptions); copyErr != nil {
		return fmt.Errorf("copying rendered files to %#q:\n%w", componentOutputDir, copyErr)
	}

	return nil
}

// writeAliasSymlink creates a sibling symlink alongside componentOutputDir at the
// URL-encoded form of componentName, pointing back at the real directory with a
// relative target.
//
// No-ops when no encoding is needed (plain ASCII names) or when the underlying
// filesystem doesn't support symlinks (e.g., in-memory test FS).
//
// Refuses to overwrite a non-symlink at the alias path — if a real component
// directory already lives there (the hypothetical 'gtk%2B' next to 'gtk+'
// case), bail with an error rather than silently destroying that component's
// rendered output. RPM names don't use '%' in practice, so this is belt-and
// suspenders.
func writeAliasSymlink(fileSystem opctx.FS, componentOutputDir, componentName string) error {
	aliasName := components.RenderedSpecDirAliasName(componentName)
	if aliasName == "" {
		return nil
	}

	linker, ok := fileSystem.(afero.Linker)
	if !ok {
		slog.Debug("Filesystem doesn't support symlinks; skipping rendered-spec alias",
			"component", componentName)

		return nil
	}

	parentDir := filepath.Dir(componentOutputDir)
	aliasPath := filepath.Join(parentDir, aliasName)

	// Inspect any existing entry at the alias path. We only ever clobber a
	// pre-existing symlink (a stale alias from a previous render); a real
	// directory or file there means a name collision with another component
	// and must be reported, not silently destroyed.
	info, lstatErr := lstatIfPossible(fileSystem, aliasPath)
	switch {
	case lstatErr == nil && info.Mode()&os.ModeSymlink == 0:
		return fmt.Errorf(
			"alias path %#q is already occupied by a non-symlink entry; refusing to overwrite",
			aliasPath)
	case lstatErr == nil:
		if removeErr := fileSystem.Remove(aliasPath); removeErr != nil {
			return fmt.Errorf("removing existing alias symlink %#q:\n%w", aliasPath, removeErr)
		}
	case !errors.Is(lstatErr, os.ErrNotExist):
		return fmt.Errorf("inspecting alias path %#q:\n%w", aliasPath, lstatErr)
	}

	// Use a relative target so the rendered tree stays portable.
	target := filepath.Base(componentOutputDir)
	if symErr := linker.SymlinkIfPossible(target, aliasPath); symErr != nil {
		return fmt.Errorf("creating alias symlink %#q -> %#q:\n%w", aliasPath, target, symErr)
	}

	return nil
}

// lstatIfPossible returns the link info at path without following symlinks, if
// the underlying filesystem supports it. Falls back to a regular Stat otherwise.
func lstatIfPossible(fileSystem opctx.FS, path string) (os.FileInfo, error) {
	if lstater, ok := fileSystem.(afero.Lstater); ok {
		info, _, err := lstater.LstatIfPossible(path)

		return info, err //nolint:wrapcheck // pass-through to the caller.
	}

	return fileSystem.Stat(path) //nolint:wrapcheck // pass-through to the caller.
}
