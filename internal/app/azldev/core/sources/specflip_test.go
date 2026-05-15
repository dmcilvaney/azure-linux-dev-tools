// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadSpec parses an in-memory spec body into a [*spec.Spec].
func loadSpec(t *testing.T, body string) *spec.Spec {
	t.Helper()

	specFile, err := spec.OpenSpec(strings.NewReader(body))
	require.NoError(t, err)

	return specFile
}

// serialize round-trips a [*spec.Spec] through [spec.Spec.Serialize] and
// returns the resulting text.
func serialize(t *testing.T, specFile *spec.Spec) string {
	t.Helper()

	buf := new(bytes.Buffer)
	require.NoError(t, specFile.Serialize(buf))

	return buf.String()
}

func TestReplaceReleaseWithAutorelease_StaticRelease(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test package.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Initial entry.
`
	expected := `Name: foo
Version: 1.0
Release: %autorelease
Summary: test
License: MIT

%description
Test package.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Initial entry.
`

	specFile := loadSpec(t, input)
	require.NoError(t, sources.ReplaceReleaseWithAutorelease(specFile))
	assert.Equal(t, expected, serialize(t, specFile))
}

func TestReplaceReleaseWithAutorelease_AlreadyAutorelease(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: %autorelease
Summary: test
License: MIT

%description
Test package.
`

	specFile := loadSpec(t, input)
	require.NoError(t, sources.ReplaceReleaseWithAutorelease(specFile))
	// Idempotent: re-flipping produces the same text.
	assert.Equal(t, input, serialize(t, specFile))
}

func TestReplaceReleaseWithAutorelease_NoReleaseTag(t *testing.T) {
	input := `Name: foo
Version: 1.0
Summary: test
License: MIT

%description
Test package.
`

	specFile := loadSpec(t, input)
	err := sources.ReplaceReleaseWithAutorelease(specFile)
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrNoSuchTag)
}

func TestReplaceChangelogBodyWithAutochangelog_StaticChangelog(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test package.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Second entry.

* Sun Dec 31 2023 Someone <s@example.com> - 0.9-1
- Initial entry.
`
	expected := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test package.

%changelog
%autochangelog
`

	specFile := loadSpec(t, input)
	require.NoError(t, sources.ReplaceChangelogBodyWithAutochangelog(specFile))
	assert.Equal(t, expected, serialize(t, specFile))
}

func TestReplaceChangelogBodyWithAutochangelog_AlreadyAutochangelog(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test package.

%changelog
%autochangelog
`

	specFile := loadSpec(t, input)
	require.NoError(t, sources.ReplaceChangelogBodyWithAutochangelog(specFile))
	// Idempotent.
	assert.Equal(t, input, serialize(t, specFile))
}

func TestReplaceChangelogBodyWithAutochangelog_NoChangelogSection(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test package.
`

	specFile := loadSpec(t, input)
	err := sources.ReplaceChangelogBodyWithAutochangelog(specFile)
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrSectionNotFound)
}

func TestExtractStaticChangelogBody_PreservesLinesVerbatim(t *testing.T) {
	input := `Name: foo
Release: 5%{?dist}

%description
desc.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Second entry.

* Sun Dec 31 2023 Someone <s@example.com> - 0.9-1
- Initial entry.
`

	expected := []string{
		"* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1",
		"- Second entry.",
		"",
		"* Sun Dec 31 2023 Someone <s@example.com> - 0.9-1",
		"- Initial entry.",
	}

	specFile := loadSpec(t, input)
	body, err := sources.ExtractStaticChangelogBody(specFile)
	require.NoError(t, err)
	assert.Equal(t, expected, body)
}

func TestExtractStaticChangelogBody_NoChangelogSection(t *testing.T) {
	input := `Name: foo
Release: 5%{?dist}

%description
desc.
`

	specFile := loadSpec(t, input)
	_, err := sources.ExtractStaticChangelogBody(specFile)
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrSectionNotFound)
}

func TestExtractThenReplaceChangelog(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test package.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Initial entry.
`

	specFile := loadSpec(t, input)

	captured, err := sources.ExtractStaticChangelogBody(specFile)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1",
		"- Initial entry.",
	}, captured)

	require.NoError(t, sources.ReplaceReleaseWithAutorelease(specFile))
	require.NoError(t, sources.ReplaceChangelogBodyWithAutochangelog(specFile))

	result := serialize(t, specFile)
	assert.Contains(t, result, "Release: %autorelease")
	assert.Contains(t, result, "%changelog\n%autochangelog\n")
}

func TestSkipChangelogMarkerConstant(t *testing.T) {
	// Verify the constant matches what rpmautospec expects.
	assert.Equal(t, "[skip changelog]", sources.SkipChangelogMarker)
}

// TestReplaceReleaseWithAutorelease_PreservesExistingArguments verifies that when a
// spec already uses %autorelease — possibly with flags like -b N (base
// release), -p (pre-release), -e EXTRAVER, -s SNAPINFO, or conditional
// fallback forms — re-flipping must NOT strip those arguments. The %autorelease
// macro is the source of truth for release semantics; rewriting it to the bare
// form silently changes the resulting NVR.
//
// The auto-detect code path is the 90% case. When a spec is already using
// %autorelease with custom args, that's an explicit author choice we must honor.
func TestReplaceReleaseWithAutorelease_PreservesExistingArguments(t *testing.T) {
	cases := []struct {
		name    string
		release string
	}{
		// rpmautospec flag forms — see rpmautospec docs.
		{"base_offset", "%autorelease -b 10"},
		{"prerelease", "%autorelease -p"},
		{"extraver", "%autorelease -e %{?extraver}"},
		{"snapinfo", "%autorelease -s %{date}git%{shortcommit}"},
		{"combined_flags", "%autorelease -p -b 5 -e asan"},
		// Braced and conditional macro forms.
		{"braced", "%{autorelease}"},
		{"braced_with_args", "%{autorelease -e asan}"},
		{"conditional", "%{?autorelease}"},
		{"conditional_fallback", "%{?autorelease}%{!?autorelease:1%{?dist}}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := "Name: foo\nVersion: 1.0\nRelease: " + tc.release + "\nSummary: t\nLicense: MIT\n\n%description\nT.\n"

			specFile := loadSpec(t, input)
			require.NoError(t, sources.ReplaceReleaseWithAutorelease(specFile))

			result := serialize(t, specFile)
			// The Release line must be preserved byte-for-byte.
			assert.Contains(t, result, "Release: "+tc.release+"\n",
				"existing %%autorelease form must be preserved, not stripped to bare %%autorelease")
		})
	}
}

// TestReplaceChangelogBodyWithAutochangelog_PreservesExistingForms verifies
// that when a spec's %changelog body already uses %autochangelog (in any form),
// re-running the flip must not modify it. This protects against losing any
// surrounding comments, alternate macro forms, or future rpmautospec features.
func TestReplaceChangelogBodyWithAutochangelog_PreservesExistingForms(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bare", "%autochangelog"},
		{"braced", "%{autochangelog}"},
		{"conditional", "%{?autochangelog}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := "Name: foo\nVersion: 1.0\nRelease: 1%{?dist}\nSummary: t\nLicense: MIT\n\n%description\nT.\n\n%changelog\n" + tc.body + "\n"

			specFile := loadSpec(t, input)
			require.NoError(t, sources.ReplaceChangelogBodyWithAutochangelog(specFile))

			assert.Equal(t, input, serialize(t, specFile),
				"existing %%autochangelog form must be preserved, not rewritten")
		})
	}
}
