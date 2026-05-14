// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
)

// autoreleasePattern matches the %autorelease macro invocation in a Release tag value.
// This covers:
//   - bare form: %autorelease
//   - braced form: %{autorelease}
//   - braced form with arguments: %{autorelease -e asan}
//   - conditional form (no fallback): %{?autorelease}
var autoreleasePattern = regexp.MustCompile(`%(\{[?]?autorelease($|[}\s])|autorelease($|\s))`)

// staticReleasePattern matches only the two Release tag forms we can safely
// auto-bump: a bare integer (e.g. "1") or an integer followed by a
// dist macro (e.g. "5%{?dist}" or "5%{dist}"). Any other suffix — dotted
// segments, unknown macros, etc. — is rejected so the component must use
// 'release.calculation = "manual"'.
var staticReleasePattern = regexp.MustCompile(`^(\d+)(%\{\??dist\})?$`)

// GetReleaseTagValue reads the Release tag value from the spec file at specPath.
// It returns the raw value string as written in the spec (e.g. "1%{?dist}" or "%autorelease").
// Returns [spec.ErrNoSuchTag] if no Release tag is found.
func GetReleaseTagValue(fs opctx.FS, specPath string) (string, error) {
	specFile, err := fs.Open(specPath)
	if err != nil {
		return "", fmt.Errorf("failed to open spec %#q:\n%w", specPath, err)
	}
	defer specFile.Close()

	openedSpec, err := spec.OpenSpec(specFile)
	if err != nil {
		return "", fmt.Errorf("failed to parse spec %#q:\n%w", specPath, err)
	}

	var releaseValue string

	err = openedSpec.VisitTagsPackage("", func(tagLine *spec.TagLine, _ *spec.Context) error {
		if strings.EqualFold(tagLine.Tag, "Release") {
			releaseValue = tagLine.Value
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to visit tags in spec %#q:\n%w", specPath, err)
	}

	if releaseValue == "" {
		return "", fmt.Errorf("release tag not found in spec %#q:\n%w", specPath, spec.ErrNoSuchTag)
	}

	return releaseValue, nil
}

// ReleaseUsesAutorelease reports whether the given Release tag value uses the
// %autorelease macro (either bare or braced form).
func ReleaseUsesAutorelease(releaseValue string) bool {
	return autoreleasePattern.MatchString(releaseValue)
}

// BumpStaticRelease increments the leading integer in a static Release tag value
// by the given commit count.
func BumpStaticRelease(releaseValue string, commitCount int) (string, error) {
	matches := staticReleasePattern.FindStringSubmatch(releaseValue)
	if matches == nil {
		return "", fmt.Errorf("release value %#q does not start with an integer", releaseValue)
	}

	currentRelease, err := strconv.Atoi(matches[1])
	if err != nil {
		return "", fmt.Errorf("failed to parse release number from %#q:\n%w", releaseValue, err)
	}

	newRelease := currentRelease + commitCount
	suffix := matches[2]

	return fmt.Sprintf("%d%s", newRelease, suffix), nil
}

// tryApplyReleaseCalculation manages the Release tag based on the component's
// release calculation mode. For non-manual modes the spec is flipped to
// %autorelease so that rpmautospec derives the release number from the
// synthetic git history. The commitCount is no longer used — release counting
// is delegated entirely to rpmautospec.
//
//   - "manual":      no-op — component manages its own release numbering.
//   - "autorelease": validates that the spec already uses %autorelease; errors
//     if not so the user can fix the mismatch.
//   - "static":      validates that the spec uses a static integer, then flips
//     it to %autorelease.
//   - "auto":        auto-detects: if the spec already uses %autorelease,
//     no-op; if it uses a static integer or non-standard value, flips to
//     %autorelease.
func (p *sourcePreparerImpl) tryApplyReleaseCalculation(
	component components.Component,
	sourcesDirPath string,
) error {
	calc := component.GetConfig().Release.Calculation

	switch calc {
	case projectconfig.ReleaseCalculationManual:
		slog.Debug("Component uses manual release calculation; skipping",
			"component", component.GetName())

		return nil

	case projectconfig.ReleaseCalculationAutorelease:
		return p.validateAutorelease(component, sourcesDirPath)

	case projectconfig.ReleaseCalculationStatic:
		return p.flipStaticToAutorelease(component, sourcesDirPath, true)

	case projectconfig.ReleaseCalculationAuto:
		return p.flipStaticToAutorelease(component, sourcesDirPath, false)

	default:
		return fmt.Errorf("component %#q has unknown release calculation mode %#q",
			component.GetName(), calc)
	}
}

// validateAutorelease checks that the spec's Release tag already uses
// %autorelease. If it doesn't, it returns an error telling the user to fix
// the mismatch (either switch to "auto"/"static" mode or update the spec).
func (p *sourcePreparerImpl) validateAutorelease(
	component components.Component,
	sourcesDirPath string,
) error {
	specPath, err := p.resolveSpecPath(component, sourcesDirPath)
	if err != nil {
		return err
	}

	releaseValue, err := GetReleaseTagValue(p.fs, specPath)
	if err != nil {
		return fmt.Errorf("failed to read Release tag for component %#q:\n%w",
			component.GetName(), err)
	}

	if !ReleaseUsesAutorelease(releaseValue) {
		return fmt.Errorf(
			"component %#q has 'release.calculation = \"autorelease\"' but its Release tag "+
				"is %#q, not %%autorelease; set 'release.calculation = \"auto\"' to flip "+
				"automatically, or update the spec",
			component.GetName(), releaseValue)
	}

	slog.Debug("Release tag already uses %%autorelease; validated",
		"component", component.GetName())

	return nil
}

// flipStaticToAutorelease reads the Release tag and flips it to %autorelease.
// When requireStatic is true (explicit "static" mode), encountering
// %autorelease produces an error. When false ("auto" mode), specs already
// using %autorelease are silently skipped.
func (p *sourcePreparerImpl) flipStaticToAutorelease(
	component components.Component,
	sourcesDirPath string,
	requireStatic bool,
) error {
	specPath, err := p.resolveSpecPath(component, sourcesDirPath)
	if err != nil {
		return err
	}

	releaseValue, err := GetReleaseTagValue(p.fs, specPath)
	if err != nil {
		return fmt.Errorf("failed to read Release tag for component %#q:\n%w",
			component.GetName(), err)
	}

	if ReleaseUsesAutorelease(releaseValue) {
		if requireStatic {
			return fmt.Errorf(
				"component %#q has 'release.calculation = \"static\"' but its Release tag "+
					"uses %%autorelease; set 'release.calculation = \"autorelease\"' instead",
				component.GetName())
		}

		slog.Debug("Spec already uses %%autorelease; skipping flip",
			"component", component.GetName())

		return nil
	}

	slog.Info("Flipping Release to %%autorelease",
		"component", component.GetName(),
		"oldRelease", releaseValue)

	overlay := projectconfig.ComponentOverlay{
		Type:  projectconfig.ComponentOverlayUpdateSpecTag,
		Tag:   "Release",
		Value: "%autorelease",
	}

	if err := ApplySpecOverlayToFileInPlace(p.fs, overlay, specPath); err != nil {
		return fmt.Errorf("failed to flip Release to %%autorelease for component %#q:\n%w",
			component.GetName(), err)
	}

	return nil
}
