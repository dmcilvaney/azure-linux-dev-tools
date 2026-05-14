// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"path/filepath"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// writeTestSpecWithChangelog writes a minimal spec with a '%changelog' section
// whose body is the given lines. An empty body slice produces a spec with the
// '%changelog' header and nothing after it.
func writeTestSpecWithChangelog(t *testing.T, memFS afero.Fs, changelogBody []string) string {
	t.Helper()

	const name = "test-pkg"

	specDir := filepath.Join(testSourcesDir, name)
	require.NoError(t, fileutils.MkdirAll(memFS, specDir))

	specPath := filepath.Join(specDir, name+".spec")

	body := "Name: " + name + "\n" +
		"Version: 1.0.0\n" +
		"Release: 1%{?dist}\n" +
		"Summary: Test\n" +
		"License: MIT\n" +
		"\n" +
		"%description\n" +
		"Test package.\n" +
		"\n" +
		"%changelog\n"

	for _, line := range changelogBody {
		body += line + "\n"
	}

	require.NoError(t, fileutils.WriteFile(memFS, specPath, []byte(body), fileperms.PublicFile))

	return specPath
}

func TestTryMaterializeStaticChangelog_ManualSkips(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	comp := mockComponent(ctrl, "kernel", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationManual,
		},
	})

	// No spec file needed — should skip before reading anything.
	err := preparer.tryMaterializeStaticChangelog(comp, testSourcesDir)
	require.NoError(t, err)
}

func TestTryMaterializeStaticChangelog_AutochangelogSkips(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationAutochangelog,
		},
	})

	// No spec file needed — should skip before reading anything.
	err := preparer.tryMaterializeStaticChangelog(comp, testSourcesDir)
	require.NoError(t, err)
}

func TestTryMaterializeStaticChangelog_AutoSkipsForAutochangelog(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	specPath := writeTestSpecWithChangelog(t, memFS, []string{"%autochangelog"})

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationAuto,
		},
	})

	err := preparer.tryMaterializeStaticChangelog(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)

	// No sidecar file should have been written.
	sidecarPath := filepath.Join(filepath.Dir(specPath), ChangelogSidecarFilename)
	exists, err := fileutils.Exists(memFS, sidecarPath)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestTryMaterializeStaticChangelog_AutoMaterializesStatic(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	specPath := writeTestSpecWithChangelog(t, memFS, []string{
		"* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1",
		"- Initial entry.",
	})

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationAuto,
		},
	})

	err := preparer.tryMaterializeStaticChangelog(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)

	// Spec should now have %autochangelog in its %changelog body.
	specContent, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(specContent), "%changelog\n%autochangelog\n")
	assert.NotContains(t, string(specContent), "Initial entry")

	// Sidecar should contain the original entries verbatim.
	sidecarPath := filepath.Join(filepath.Dir(specPath), ChangelogSidecarFilename)
	sidecarContent, err := fileutils.ReadFile(memFS, sidecarPath)
	require.NoError(t, err)
	assert.Equal(t,
		"* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1\n- Initial entry.\n",
		string(sidecarContent))
}

func TestTryMaterializeStaticChangelog_ExplicitStaticMaterializes(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	specPath := writeTestSpecWithChangelog(t, memFS, []string{
		"* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1",
		"- Initial entry.",
	})

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationStatic,
		},
	})

	err := preparer.tryMaterializeStaticChangelog(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)

	specContent, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(specContent), "%changelog\n%autochangelog\n")

	sidecarPath := filepath.Join(filepath.Dir(specPath), ChangelogSidecarFilename)
	exists, err := fileutils.Exists(memFS, sidecarPath)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestTryMaterializeStaticChangelog_ExplicitStaticErrorsOnAutochangelog(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpecWithChangelog(t, memFS, []string{"%autochangelog"})

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationStatic,
		},
	})

	err := preparer.tryMaterializeStaticChangelog(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `changelog.calculation = "autochangelog"`)
}

func TestTryMaterializeStaticChangelog_ExplicitStaticErrorsOnMissingSection(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	// Spec has no %changelog section at all.
	writeTestSpec(t, memFS, "test-pkg", "1%{?dist}")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationStatic,
		},
	})

	err := preparer.tryMaterializeStaticChangelog(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no %changelog section")
}

func TestTryMaterializeStaticChangelog_AutoSkipsWhenNoChangelogSection(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "test-pkg", "1%{?dist}")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Changelog: projectconfig.ChangelogConfig{
			Calculation: projectconfig.ChangelogCalculationAuto,
		},
	})

	err := preparer.tryMaterializeStaticChangelog(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)
}

func TestChangelogBodyUsesAutochangelog(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		body     []string
		expected bool
	}{
		{"bare", []string{"%autochangelog"}, true},
		{"braced", []string{"%{autochangelog}"}, true},
		{"conditional", []string{"%{?autochangelog}"}, true},
		{"with leading blanks", []string{"", "", "%autochangelog"}, true},
		{"static entries", []string{"* Mon Jan 01 2024 Someone - 1-1", "- entry."}, false},
		{"empty", nil, false},
		{"only blanks", []string{"", "  ", ""}, false},
		{"false positive macro", []string{"%{autochangelog_suffix}"}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.expected, changelogBodyUsesAutochangelog(testCase.body))
		})
	}
}
