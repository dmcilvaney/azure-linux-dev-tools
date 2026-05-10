// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
)

// resolveAndValidateOutputDir resolves the output directory from CLI flags and
// project config. If neither the config nor --output-dir provides a path, an
// error is returned. When the output dir comes from config, --force is auto-set
// to allow overwriting component output (the configured path is trusted).
func resolveAndValidateOutputDir(env *azldev.Env, options *Options) error {
	configDir := env.Config().Project.RenderedSpecsDir

	switch {
	case options.OutputDirExplicit:
		// CLI flag wins — use as-is.
	case configDir != "":
		// Config provides the output dir; auto-trust it for overwrites.
		options.OutputDir = configDir
		options.Force = true
	default:
		return errors.New(
			"no output directory configured; set rendered-specs-dir in the project config " +
				"or pass --output-dir (-o) on the command line")
	}

	return validateOutputDir(options.OutputDir)
}

// validateOutputDir rejects output directory values that could cause the
// --clean-stale wipe to delete unrelated directories.
func validateOutputDir(outputDir string) error {
	cleaned := filepath.Clean(outputDir)
	if cleaned == "." || cleaned == string(filepath.Separator) ||
		cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf(
			"output directory %#q is unsafe; use a dedicated subdirectory (e.g., ./SPECS/)", outputDir)
	}

	return nil
}

// validateCleanStaleOptions enforces the constraints around --clean-stale.
// Extracted from RenderComponents to keep its complexity below the linter's
// cyclomatic threshold.
func validateCleanStaleOptions(options *Options) error {
	if !options.CleanStale {
		return nil
	}

	if !options.ComponentFilter.IncludeAllComponents {
		return errors.New("--clean-stale requires -a (render all components)")
	}

	if options.OutputDirExplicit && !options.Force {
		return errors.New("--clean-stale with --output-dir requires --force (-f)")
	}

	return nil
}

// createMockProcessor creates a [sources.MockProcessor] using the project's
// mock config. Returns nil if the mock config is not available (e.g., no project
// config loaded, or no mock config path configured).
func createMockProcessor(env *azldev.Env) *sources.MockProcessor {
	_, distroVerDef, err := env.Distro()
	if err != nil {
		slog.Info("Mock processor unavailable; could not resolve distro", "error", err)

		return nil
	}

	if distroVerDef.MockConfigPath == "" {
		slog.Info("Mock processor unavailable; no mock config path configured")

		return nil
	}

	slog.Info("Mock processor available", "mockConfig", distroVerDef.MockConfigPath)

	return sources.NewMockProcessor(env, distroVerDef.MockConfigPath)
}
