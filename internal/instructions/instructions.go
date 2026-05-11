// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package instructions provisions AI-agent instruction files (Copilot,
// Copilot CLI, AGENTS.md, ...) into a project repository or a user's
// global configuration directory.
//
// The set of files is embedded into the azldev binary so that
// downstream consumers can keep them in sync simply by re-running
// `azldev instructions install`.
package instructions

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// embedfsRootDir is the root directory inside [content] that contains
// the layout to provision.
const embedfsRootDir = "content"

// The embedded filesystem holding the placeholder instruction files.
// Use the `all:` prefix so dot-prefixed paths like `.github/` are included.
//
//go:embed all:content
var content embed.FS

// Scope identifies where a set of instruction files should be installed.
type Scope int

const (
	// ScopeProject installs into a project repository (typically the
	// current working directory). Files land at their conventional
	// repo-relative paths (e.g. `.github/copilot-instructions.md`,
	// `AGENTS.md`).
	ScopeProject Scope = iota
	// ScopeUser installs into the user's per-user configuration
	// directory under an `azldev/instructions/` subtree. The same
	// repo-relative layout is mirrored inside that subtree so the
	// user can move or symlink individual files into the location
	// their tool of choice expects.
	ScopeUser
)

// String implements [fmt.Stringer].
func (s Scope) String() string {
	switch s {
	case ScopeProject:
		return "project"
	case ScopeUser:
		return "user"
	default:
		return fmt.Sprintf("Scope(%d)", int(s))
	}
}

// Options controls how [Provision] writes files.
type Options struct {
	// Force, when true, overwrites existing files. Without it,
	// existing files are skipped (and reported via [Result.Skipped]).
	Force bool
	// AssumeYes, when true, suppresses any interactive confirmation
	// prompts and proceeds with the safe default (skip pre-existing
	// files unless [Force] is also set).
	AssumeYes bool
}

// Result summarizes the outcome of a single [Provision] call.
type Result struct {
	// DestBase is the absolute base path under which files were
	// written. For [ScopeProject] this is the project root; for
	// [ScopeUser] it is the per-user config directory subtree.
	DestBase string
	// Written lists relative paths (under [DestBase]) of files that
	// were newly written or overwritten.
	Written []string
	// Skipped lists relative paths (under [DestBase]) of files that
	// were left untouched because they already existed and [Options.Force]
	// was not set.
	Skipped []string
}

// Files returns the set of relative paths (under [embedfsRootDir]) of
// every embedded instruction file. Useful for tests and for
// presenting a preview to the user.
func Files() ([]string, error) {
	var files []string

	walkErr := fs.WalkDir(content, embedfsRootDir, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		// embed.FS always uses forward slashes regardless of host OS, so use
		// the slash-aware [path] package (not [filepath]) to compute the
		// path relative to embedfsRootDir.
		rel, relErr := relSlash(embedfsRootDir, walkPath)
		if relErr != nil {
			return fmt.Errorf("failed to compute relative path for %#q:\n%w", walkPath, relErr)
		}

		files = append(files, rel)

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to enumerate embedded instruction files:\n%w", walkErr)
	}

	return files, nil
}

// Provision writes the embedded instruction files to destBase.
//
// destBase must be an absolute path; it will be created if missing.
// The set of files written is identical regardless of scope; scope is
// retained on [Result] purely for caller reporting.
func Provision(
	dryRunnable opctx.DryRunnable,
	destFS opctx.FS,
	scope Scope,
	destBase string,
	options Options,
) (*Result, error) {
	if !filepath.IsAbs(destBase) {
		return nil, fmt.Errorf("destination path %#q must be absolute", destBase)
	}

	relPaths, err := Files()
	if err != nil {
		return nil, err
	}

	result := &Result{
		DestBase: destBase,
	}

	if dryRunnable.DryRun() {
		slog.Info("Dry run; would provision instruction files",
			"scope", scope.String(), "destBase", destBase, "count", len(relPaths))

		// In dry-run mode we still report what *would* be written so
		// the caller can show the user a meaningful preview.
		result.Written = append(result.Written, relPaths...)

		return result, nil
	}

	for _, rel := range relPaths {
		written, writeErr := writeOne(destFS, destBase, rel, options.Force)
		if writeErr != nil {
			return result, writeErr
		}

		if written {
			result.Written = append(result.Written, rel)
		} else {
			result.Skipped = append(result.Skipped, rel)
		}
	}

	return result, nil
}

// writeOne writes a single embedded file to destBase/rel. Returns true
// if the file was written, false if it was skipped because it already
// existed and force was not set.
func writeOne(destFS opctx.FS, destBase, rel string, force bool) (bool, error) {
	// rel uses forward slashes (slash-separated relative path) — convert to OS native.
	destPath := filepath.Join(destBase, filepath.FromSlash(rel))

	// Check for an existing file.
	if !force {
		exists, existsErr := fileutils.Exists(destFS, destPath)
		if existsErr != nil {
			return false, fmt.Errorf("failed to check if %#q exists:\n%w", destPath, existsErr)
		}

		if exists {
			slog.Debug("Instruction file already exists; skipping (use --force to overwrite)",
				"path", destPath)

			return false, nil
		}
	}

	// Make sure the destination directory exists.
	destDir := filepath.Dir(destPath)
	if dirErr := fileutils.MkdirAll(destFS, destDir); dirErr != nil {
		return false, fmt.Errorf("failed to create directory %#q:\n%w", destDir, dirErr)
	}

	// Read embedded source. embed.FS uses forward slashes regardless of host OS.
	srcPath := path.Join(embedfsRootDir, rel)

	data, readErr := content.ReadFile(srcPath)
	if readErr != nil {
		return false, fmt.Errorf("failed to read embedded instruction file %#q:\n%w", srcPath, readErr)
	}

	if writeErr := fileutils.WriteFile(destFS, destPath, data, fileperms.PublicFile); writeErr != nil {
		return false, fmt.Errorf("failed to write %#q:\n%w", destPath, writeErr)
	}

	slog.Info("Wrote instruction file", "path", destPath)

	return true, nil
}

// relSlash returns the slash-separated path of target relative to base.
// It assumes both inputs are already slash-separated (as produced by
// the [embed.FS] / [io/fs] APIs) and rejects target paths that are not
// prefixed by base.
func relSlash(base, target string) (string, error) {
	prefix := base
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	if target == base {
		return ".", nil
	}

	rel, ok := strings.CutPrefix(target, prefix)
	if !ok {
		return "", fmt.Errorf("path %#q is not under base %#q", target, base)
	}

	return rel, nil
}

// ErrNoUserConfigDir is returned when [UserDestBase] cannot determine the
// user's per-user configuration directory.
var ErrNoUserConfigDir = errors.New("could not determine user configuration directory")

// UserDestBase returns the per-user destination base path. It honors
// XDG_CONFIG_HOME, then HOME (`~/.config`), then USERPROFILE on
// Windows, in the order common to other CLI tools. The returned path
// is always absolute.
func UserDestBase(env opctx.OSEnv) (string, error) {
	// Sub-directory under the user's config dir.
	const subDir = "azldev/instructions"

	if xdg := strings.TrimSpace(env.Getenv("XDG_CONFIG_HOME")); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, filepath.FromSlash(subDir)), nil
	}

	if home := strings.TrimSpace(env.Getenv("HOME")); home != "" && filepath.IsAbs(home) {
		return filepath.Join(home, ".config", filepath.FromSlash(subDir)), nil
	}

	if up := strings.TrimSpace(env.Getenv("USERPROFILE")); up != "" && filepath.IsAbs(up) {
		return filepath.Join(up, ".config", filepath.FromSlash(subDir)), nil
	}

	return "", ErrNoUserConfigDir
}
