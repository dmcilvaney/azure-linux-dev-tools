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

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
	importCommit string,
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
		return p.materializeStaticChangelog(component, sourcesDirPath, importCommit, true)

	case projectconfig.ChangelogCalculationAuto, "":
		return p.materializeStaticChangelog(component, sourcesDirPath, importCommit, false)

	default:
		return fmt.Errorf("component %#q has unknown changelog calculation mode %#q",
			component.GetName(), calc)
	}
}

// materializeStaticChangelog reads the spec, captures the static '%changelog'
// body, writes it to a sidecar 'changelog' file next to the spec, and
// rewrites the spec's body to a single '%autochangelog' line.
//
// The sidecar content is taken from the import-commit's spec '%changelog'
// body when available (so it represents only the entries strictly older than
// the seed commit where rpmautospec stops its dynamic walk). When the
// import-commit cannot be read (empty hash, no sources git repo, commit
// missing, spec missing, parse error) the working-dir spec body is used as a
// fallback. Without this boundary, every entry the dynamic walk regenerates
// from synthetic+replayed-upstream commits would also appear verbatim in the
// static sidecar portion below it, producing duplicates in the rendered
// '%changelog'.
//
// When requireStatic is true (explicit "static" mode), encountering a spec
// that uses '%autochangelog' or has no '%changelog' section produces an
// error pointing the user at the correct calculation value. When false
// ("auto" mode), those cases are silently skipped.
func (p *sourcePreparerImpl) materializeStaticChangelog(
	component components.Component,
	sourcesDirPath string,
	importCommit string,
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

	// Prefer the import-commit's spec body for the sidecar so the sidecar
	// only contains entries strictly older than the rpmautospec walk
	// boundary. Fall back to the working-dir body when unavailable.
	sidecarBody, sidecarSource := pickSidecarBody(sourcesDirPath, importCommit, filepath.Base(specPath), body)

	sidecarPath := filepath.Join(filepath.Dir(specPath), ChangelogSidecarFilename)
	if err := writeChangelogSidecar(p.fs, sidecarPath, sidecarBody); err != nil {
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
		"preservedEntryLines", len(sidecarBody),
		"sidecarSource", sidecarSource,
		"sidecar", sidecarPath)

	return nil
}

// pickSidecarBody chooses the changelog body to write into the sidecar.
// When importCommit is non-empty and the sources git repo + import-commit's
// spec are readable, returns that body (tagged "import-commit"); otherwise
// returns the working-dir body unchanged (tagged "working-dir"). The
// source tag is for observability only.
func pickSidecarBody(
	sourcesDirPath, importCommit, specBasename string,
	workingBody []string,
) ([]string, string) {
	if importCommit == "" {
		return workingBody, "working-dir"
	}

	importBody, err := extractChangelogBodyFromImportCommit(sourcesDirPath, importCommit, specBasename)
	if err != nil {
		slog.Debug("Falling back to working-dir spec body for sidecar; cannot read import-commit spec",
			"importCommit", importCommit, "err", err)

		return workingBody, "working-dir"
	}

	return importBody, "import-commit"
}

// extractChangelogBodyFromImportCommit opens the sources git repo, reads the
// spec file at the import-commit's tree (looked up by basename), and returns
// its '%changelog' body. Errors are returned verbatim; callers are expected
// to treat them as fallback signals, not hard failures.
func extractChangelogBodyFromImportCommit(
	sourcesDirPath, importCommit, specBasename string,
) ([]string, error) {
	repo, err := gogit.PlainOpen(sourcesDirPath)
	if err != nil {
		return nil, fmt.Errorf("open sources git repo at %#q:\n%w", sourcesDirPath, err)
	}

	commit, err := repo.CommitObject(plumbing.NewHash(importCommit))
	if err != nil {
		return nil, fmt.Errorf("look up import commit %#q:\n%w", importCommit, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read import commit tree:\n%w", err)
	}

	file, err := tree.File(specBasename)
	if err != nil {
		return nil, fmt.Errorf("find spec %#q in import tree:\n%w", specBasename, err)
	}

	reader, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("read spec blob from import tree:\n%w", err)
	}
	defer reader.Close()

	specFile, err := spec.OpenSpec(reader)
	if err != nil {
		return nil, fmt.Errorf("parse spec from import tree:\n%w", err)
	}

	body, err := ExtractStaticChangelogBody(specFile)
	if err != nil {
		return nil, fmt.Errorf("extract %%changelog body from import spec:\n%w", err)
	}

	return body, nil
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
