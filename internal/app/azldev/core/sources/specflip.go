// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"fmt"

	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
)

// SkipChangelogMarker is the rpmautospec magic comment that excludes a commit
// from %autochangelog materialization while still counting it for %autorelease.
// Must appear on its own line in the commit message body (rpmautospec's
// magic_comment_re is line-anchored: ^\s*\[(?P<magic>.*)\]\s*$).
const SkipChangelogMarker = "[skip changelog]"

// FlipReleaseToAutorelease rewrites the first 'Release:' tag value to
// '%autorelease' so that rpmautospec process-distgit resolves the release
// number from the synthetic git history. If the spec has no 'Release:' tag,
// [spec.ErrNoSuchTag] is returned. Idempotent: re-flipping a spec that
// already uses %autorelease is a no-op.
func FlipReleaseToAutorelease(specFile *spec.Spec) error {
	if err := specFile.UpdateExistingTag("", "Release", "%autorelease"); err != nil {
		return fmt.Errorf("failed to flip Release tag to %%autorelease:\n%w", err)
	}

	return nil
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
// Idempotent: re-running on a spec already using %autochangelog is a no-op.
func ReplaceChangelogBodyWithAutochangelog(specFile *spec.Spec) error {
	hasChangelog, err := specFile.HasSection("%changelog")
	if err != nil {
		return fmt.Errorf("failed to scan spec for %%changelog section:\n%w", err)
	}

	if !hasChangelog {
		return fmt.Errorf("spec has no %%changelog section:\n%w", spec.ErrSectionNotFound)
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

// ExtractStaticChangelogBody returns the body lines of the '%changelog'
// section (excluding the '%changelog' header itself) as they appear in the
// spec, in order. Empty lines within the section are preserved verbatim.
//
// Callers use this to capture pre-existing static entries before flipping
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
