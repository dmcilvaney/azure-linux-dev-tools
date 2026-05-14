// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// ChangelogSidecarFilename is the name of the file rpmautospec reads to
// preserve curated pre-import '%changelog' entries. Its contents are appended
// below entries materialized from git history when the spec uses
// '%autochangelog'.
const ChangelogSidecarFilename = "changelog"

// autochangelogPattern matches the %autochangelog macro invocation in a
// '%changelog' body line. Mirrors [autoreleasePattern] and covers:
//   - bare form:               %autochangelog
//   - braced form:             %{autochangelog}
//   - braced with arguments:   %{autochangelog -e args}
//   - conditional (no else):   %{?autochangelog}
var autochangelogPattern = regexp.MustCompile(`%(\{[?]?autochangelog($|[}\s])|autochangelog($|\s))`)

// tryMaterializeStaticChangelog manages the spec's '%changelog' section based
// on the component's changelog calculation mode. It may flip a static body to
// '%autochangelog' (writing the preserved entries to a sidecar 'changelog'
// file next to the spec) or leave the spec untouched.
//
//   - "manual":        no-op — component manages its own %changelog.
//   - "autochangelog": no-op — spec already uses %autochangelog; rpmautospec
//     materializes entries from git history.
//   - "static":        always extracts the static body, writes it to the
//     sidecar 'changelog' file, and replaces the body with %autochangelog.
//     Errors if the spec already uses %autochangelog or has no %changelog.
//   - "auto":          auto-detects from the spec; skips if %autochangelog is
//     found, otherwise behaves like "static".
func (p *sourcePreparerImpl) tryMaterializeStaticChangelog(
	component components.Component,
	sourcesDirPath string,
) error {
	calc := component.GetConfig().Changelog.Calculation

	switch calc {
	case projectconfig.ChangelogCalculationManual:
		slog.Debug("Component uses manual changelog calculation; skipping",
			"component", component.GetName())

		return nil

	case projectconfig.ChangelogCalculationAutochangelog:
		slog.Debug("Component uses autochangelog calculation; skipping",
			"component", component.GetName())

		return nil

	case projectconfig.ChangelogCalculationStatic:
		return p.materializeStaticChangelog(component, sourcesDirPath, true)

	case projectconfig.ChangelogCalculationAuto, "":
		return p.materializeStaticChangelog(component, sourcesDirPath, false)

	default:
		return fmt.Errorf("component %#q has unknown changelog calculation mode %#q",
			component.GetName(), calc)
	}
}

// materializeStaticChangelog reads the spec, captures the static '%changelog'
// body, writes it to a sidecar 'changelog' file next to the spec, and
// rewrites the spec's body to a single '%autochangelog' line.
//
// When requireStatic is true (explicit "static" mode), encountering a spec
// that uses '%autochangelog' or has no '%changelog' section produces an
// error pointing the user at the correct calculation value. When false
// ("auto" mode), those cases are silently skipped.
func (p *sourcePreparerImpl) materializeStaticChangelog(
	component components.Component,
	sourcesDirPath string,
	requireStatic bool,
) error {
	specPath, err := p.resolveSpecPath(component, sourcesDirPath)
	if err != nil {
		return err
	}

	specFile, err := openSpecFromFS(p.fs, specPath)
	if err != nil {
		return fmt.Errorf("failed to read spec for component %#q:\n%w",
			component.GetName(), err)
	}

	body, err := ExtractStaticChangelogBody(specFile)

	switch {
	case errors.Is(err, spec.ErrSectionNotFound):
		if requireStatic {
			return fmt.Errorf(
				"component %#q has 'changelog.calculation = \"static\"' but its spec has no %%changelog section",
				component.GetName())
		}

		slog.Debug("Spec has no %%changelog section; skipping static changelog materialization",
			"component", component.GetName())

		return nil

	case err != nil:
		return fmt.Errorf("failed to extract %%changelog body for component %#q:\n%w",
			component.GetName(), err)
	}

	if changelogBodyUsesAutochangelog(body) {
		if requireStatic {
			return fmt.Errorf(
				"component %#q has 'changelog.calculation = \"static\"' but its %%changelog body "+
					"uses %%autochangelog; set 'changelog.calculation = \"autochangelog\"' instead",
				component.GetName())
		}

		slog.Debug("Spec uses %%autochangelog; skipping static changelog materialization",
			"component", component.GetName())

		return nil
	}

	sidecarPath := filepath.Join(filepath.Dir(specPath), ChangelogSidecarFilename)
	if err := writeChangelogSidecar(p.fs, sidecarPath, body); err != nil {
		return fmt.Errorf(
			"failed to write %#q sidecar for component %#q:\n%w",
			ChangelogSidecarFilename, component.GetName(), err)
	}

	if err := ReplaceChangelogBodyWithAutochangelog(specFile); err != nil {
		return fmt.Errorf("failed to flip %%changelog body for component %#q:\n%w",
			component.GetName(), err)
	}

	if err := writeSpecToFS(p.fs, specPath, specFile); err != nil {
		return fmt.Errorf("failed to write flipped spec for component %#q:\n%w",
			component.GetName(), err)
	}

	slog.Info("Materialized static %%changelog via rpmautospec",
		"component", component.GetName(),
		"preservedEntryLines", len(body),
		"sidecar", sidecarPath)

	return nil
}

// changelogBodyUsesAutochangelog reports whether the first non-blank line in
// a '%changelog' body invokes the '%autochangelog' macro.
func changelogBodyUsesAutochangelog(body []string) bool {
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		return autochangelogPattern.MatchString(trimmed)
	}

	return false
}

// writeChangelogSidecar writes the preserved '%changelog' body lines to the
// given path, separated by newlines and terminated by a single trailing
// newline. The empty case is allowed and produces an empty file — rpmautospec
// tolerates that.
func writeChangelogSidecar(fs opctx.FS, path string, body []string) error {
	content := strings.Join(body, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	if err := fileutils.WriteFile(fs, path, []byte(content), fileperms.PublicFile); err != nil {
		return fmt.Errorf("failed to write %#q:\n%w", path, err)
	}

	return nil
}

// openSpecFromFS reads a spec file from the given filesystem and parses it.
func openSpecFromFS(fs opctx.FS, specPath string) (*spec.Spec, error) {
	reader, err := fs.Open(specPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open spec %#q:\n%w", specPath, err)
	}
	defer reader.Close()

	specFile, err := spec.OpenSpec(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse spec %#q:\n%w", specPath, err)
	}

	return specFile, nil
}

// writeSpecToFS serializes the given spec back to the given path.
func writeSpecToFS(fs opctx.FS, specPath string, specFile *spec.Spec) error {
	buf := new(bytes.Buffer)
	if err := specFile.Serialize(buf); err != nil {
		return fmt.Errorf("failed to serialize spec %#q:\n%w", specPath, err)
	}

	if err := fileutils.WriteFile(fs, specPath, buf.Bytes(), fileperms.PublicFile); err != nil {
		return fmt.Errorf("failed to write spec %#q:\n%w", specPath, err)
	}

	return nil
}
