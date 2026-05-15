// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
)

// SkipChangelogMarker is the rpmautospec magic comment that excludes a commit
// from %autochangelog materialization while still counting it for %autorelease.
// Must appear on its own line in the commit message body (rpmautospec's
// magic_comment_re is line-anchored: ^\s*\[(?P<magic>.*)\]\s*$).
const SkipChangelogMarker = "[skip changelog]"

// autoreleaseValuePattern matches any %autorelease invocation form in a
// Release tag value: bare (%autorelease), braced (%{autorelease ...}),
// conditional (%{?autorelease}). Used by [ReplaceReleaseWithAutorelease] to detect
// values that should be left untouched so that author-specified flags
// (-b N, -p, -e EXTRAVER, -s SNAPINFO) and macro wrappers are preserved.
var autoreleaseValuePattern = regexp.MustCompile(`%(\{[?]?autorelease($|[}\s])|autorelease($|\s))`)

// autochangelogBodyPattern matches the %autochangelog macro invocation in a
// '%changelog' body line. Used by [ReplaceChangelogBodyWithAutochangelog] to
// detect bodies that should be left untouched.
var autochangelogBodyPattern = regexp.MustCompile(`%(\{[?]?autochangelog($|[}\s])|autochangelog($|\s))`)

// ReplaceReleaseWithAutorelease rewrites the first 'Release:' tag value to
// '%autorelease' so that rpmautospec process-distgit resolves the release
// number from the synthetic git history. If the spec has no 'Release:' tag,
// [spec.ErrNoSuchTag] is returned.
//
// Idempotent: if the existing Release value already invokes %autorelease in
// any form (bare, braced, conditional, with flags like -b/-p/-e/-s, or via a
// macro wrapper), the spec is left untouched. This preserves author-specified
// arguments that change the resulting NVR.
func ReplaceReleaseWithAutorelease(specFile *spec.Spec) error {
	currentValue, err := readReleaseTagValue(specFile)
	if err != nil {
		return err
	}

	if autoreleaseValuePattern.MatchString(currentValue) {
		// Already uses %autorelease; preserve existing form and arguments.
		return nil
	}

	if err := specFile.UpdateExistingTag("", "Release", "%autorelease"); err != nil {
		return fmt.Errorf("failed to replace Release tag with %%autorelease:\n%w", err)
	}

	return nil
}

// readReleaseTagValue returns the value of the first 'Release:' tag in the
// spec, or [spec.ErrNoSuchTag] if no such tag exists.
func readReleaseTagValue(specFile *spec.Spec) (string, error) {
	var value string

	err := specFile.VisitTagsPackage("", func(tagLine *spec.TagLine, _ *spec.Context) error {
		if value == "" && strings.EqualFold(tagLine.Tag, "Release") {
			value = tagLine.Value
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to read Release tag:\n%w", err)
	}

	if value == "" {
		return "", fmt.Errorf("Release tag not found:\n%w", spec.ErrNoSuchTag)
	}

	return value, nil
}

// ReplaceChangelogBodyWithAutochangelog rewrites the body of the '%changelog'
// section so that it contains a single '%autochangelog' line.
//
// This is typically paired with writing the captured static entries (see
// [ExtractStaticChangelogBody]) into a 'changelog' file alongside the spec
// in the synthetic dist-git. rpmautospec process-distgit then materializes
// synthetic-history entries in place of '%autochangelog' and appends the
// 'changelog' file contents below them.
//
// Returns [spec.ErrSectionNotFound] if the spec has no '%changelog' section.
// Idempotent: if the body already invokes %autochangelog in any form (bare,
// braced, conditional), the spec is left untouched so the existing form is
// preserved.
func ReplaceChangelogBodyWithAutochangelog(specFile *spec.Spec) error {
	hasChangelog, err := specFile.HasSection("%changelog")
	if err != nil {
		return fmt.Errorf("failed to scan spec for %%changelog section:\n%w", err)
	}

	if !hasChangelog {
		return fmt.Errorf("spec has no %%changelog section:\n%w", spec.ErrSectionNotFound)
	}

	bodyAlreadyUsesAutochangelog, err := specBodyHasAutochangelog(specFile)
	if err != nil {
		return err
	}

	if bodyAlreadyUsesAutochangelog {
		return nil
	}

	// Clear all existing body lines in the %changelog section.
	err = specFile.Visit(func(ctx *spec.Context) error {
		if ctx.Target.TargetType != spec.SectionLineTarget {
			return nil
		}

		if ctx.CurrentSection.SectName != "%changelog" {
			return nil
		}

		ctx.RemoveLine()

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to clear %%changelog body:\n%w", err)
	}

	// Insert the single replacement body line.
	if err := specFile.AppendLinesToSection("%changelog", "", []string{"%autochangelog"}); err != nil {
		return fmt.Errorf("failed to insert %%autochangelog body line:\n%w", err)
	}

	return nil
}

// specBodyHasAutochangelog reports whether the '%changelog' section's body
// already contains a line invoking %autochangelog (in any form). Empty or
// whitespace-only lines are ignored.
func specBodyHasAutochangelog(specFile *spec.Spec) (bool, error) {
	var usesAutochangelog bool

	err := specFile.Visit(func(ctx *spec.Context) error {
		if ctx.Target.TargetType != spec.SectionLineTarget {
			return nil
		}

		if ctx.CurrentSection.SectName != "%changelog" {
			return nil
		}

		if autochangelogBodyPattern.MatchString(ctx.Target.Line.Text) {
			usesAutochangelog = true
		}

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to scan %%changelog body:\n%w", err)
	}

	return usesAutochangelog, nil
}

// ExtractStaticChangelogBody returns the body lines of the '%changelog'
// section (excluding the '%changelog' header itself) as they appear in the
// spec, in order. Empty lines within the section are preserved verbatim.
//
// Callers use this to capture pre-existing static entries before replacing them with
// the spec to '%autochangelog', so the captured lines can be written to a
// 'changelog' sidecar file in the synthetic dist-git.
//
// Returns [spec.ErrSectionNotFound] if the spec has no '%changelog' section.
func ExtractStaticChangelogBody(specFile *spec.Spec) ([]string, error) {
	hasChangelog, err := specFile.HasSection("%changelog")
	if err != nil {
		return nil, fmt.Errorf("failed to scan spec for %%changelog section:\n%w", err)
	}

	if !hasChangelog {
		return nil, fmt.Errorf("spec has no %%changelog section:\n%w", spec.ErrSectionNotFound)
	}

	var body []string

	err = specFile.Visit(func(ctx *spec.Context) error {
		if ctx.Target.TargetType != spec.SectionLineTarget {
			return nil
		}

		if ctx.CurrentSection.SectName != "%changelog" {
			return nil
		}

		body = append(body, ctx.Target.Line.Text)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to visit spec for %%changelog body:\n%w", err)
	}

	return body, nil
}
