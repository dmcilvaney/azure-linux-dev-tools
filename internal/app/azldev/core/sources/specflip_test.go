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

func TestFlipReleaseToAutorelease_StaticRelease(t *testing.T) {
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
	require.NoError(t, sources.FlipReleaseToAutorelease(specFile))
	assert.Equal(t, expected, serialize(t, specFile))
}

func TestFlipReleaseToAutorelease_AlreadyAutorelease(t *testing.T) {
	input := `Name: foo
Version: 1.0
Release: %autorelease
Summary: test
License: MIT

%description
Test package.
`

	specFile := loadSpec(t, input)
	require.NoError(t, sources.FlipReleaseToAutorelease(specFile))
	// Idempotent: re-flipping a spec that already uses %autorelease produces
	// the same text.
	assert.Equal(t, input, serialize(t, specFile))
}

func TestFlipReleaseToAutorelease_NoReleaseTag(t *testing.T) {
	input := `Name: foo
Version: 1.0
Summary: test
License: MIT

%description
Test package.
`

	specFile := loadSpec(t, input)
	err := sources.FlipReleaseToAutorelease(specFile)
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
	// Idempotent: re-running on a spec that already uses %autochangelog
	// produces the same text.
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

// TestExtractThenReplaceChangelog exercises the typical composition on an
// in-memory spec: capture the static entries (so the caller can write them
// to a 'changelog' file in the synthetic dist-git) and then replace the
// body with '%autochangelog' so rpmautospec process-distgit takes over
// materialization.
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

	require.NoError(t, sources.FlipReleaseToAutorelease(specFile))
	require.NoError(t, sources.ReplaceChangelogBodyWithAutochangelog(specFile))

	// The staged spec is now ready for rpmautospec process-distgit: Release
	// flipped to %autorelease, %changelog body collapsed to %autochangelog.
	assert.Contains(t, serialize(t, specFile), "Release: %autorelease")
	assert.Contains(t, serialize(t, specFile), "%changelog\n%autochangelog\n")
}
