// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"path/filepath"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components/components_testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

const testSourcesDir = "/sources"

func newTestPreparer(memFS afero.Fs) *sourcePreparerImpl {
	return &sourcePreparerImpl{
		fs: memFS,
	}
}

func writeTestSpec(t *testing.T, memFS afero.Fs, name, release string) {
	t.Helper()

	specDir := filepath.Join(testSourcesDir, name)
	require.NoError(t, fileutils.MkdirAll(memFS, specDir))

	specPath := filepath.Join(specDir, name+".spec")
	content := []byte("Name: " + name + "\nVersion: 1.0.0\nRelease: " + release + "\nSummary: Test\nLicense: MIT\n")

	require.NoError(t, fileutils.WriteFile(memFS, specPath, content, fileperms.PublicFile))
}

func mockComponent(
	ctrl *gomock.Controller, name string, config *projectconfig.ComponentConfig,
) *components_testutils.MockComponent {
	comp := components_testutils.NewMockComponent(ctrl)
	comp.EXPECT().GetName().AnyTimes().Return(name)
	comp.EXPECT().GetConfig().AnyTimes().Return(config)

	return comp
}

func TestTryApplyReleaseCalculation_ManualSkips(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	comp := mockComponent(ctrl, "kernel", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationManual,
		},
	})

	// No spec file needed — should skip before reading anything.
	err := preparer.tryApplyReleaseCalculation(comp, testSourcesDir)
	require.NoError(t, err)
}

func TestTryApplyReleaseCalculation_AutoreleaseSkips(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "test-pkg", "%autorelease")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAuto,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)
}

func TestTryApplyReleaseCalculation_AutoReplacesStatic(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "test-pkg", "1%{?dist}")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAuto,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)

	// Verify the spec was updated.
	specPath := filepath.Join(testSourcesDir, "test-pkg", "test-pkg.spec")
	content, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Release: %autorelease")
}

func TestTryApplyReleaseCalculation_AutoReplacesStaticNoDist(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "test-pkg", "1%{dist}")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAuto,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)

	specPath := filepath.Join(testSourcesDir, "test-pkg", "test-pkg.spec")
	content, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Release: %autorelease")
}

func TestTryApplyReleaseCalculation_NonStandardErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "kernel", "%{pkg_release}")

	comp := mockComponent(ctrl, "kernel", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAuto,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "kernel"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be")
	assert.Contains(t, err.Error(), "release.calculation")
}

func TestTryApplyReleaseCalculation_ManualAcceptsNonStandard(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "kernel", "%{pkg_release}")

	comp := mockComponent(ctrl, "kernel", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationManual,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "kernel"))
	require.NoError(t, err)
}

func TestTryApplyReleaseCalculation_ExplicitAutoreleaseSkips(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	// No spec file needed — autorelease mode trusts the config declaration
	// and skips without reading the spec (supports macro indirection).
	comp := mockComponent(ctrl, "gvisor", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAutorelease,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, testSourcesDir)
	require.NoError(t, err)
}

func TestTryApplyReleaseCalculation_ExplicitStaticReplaces(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "test-pkg", "1%{?dist}")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationStatic,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.NoError(t, err)

	// Verify the spec was updated.
	specPath := filepath.Join(testSourcesDir, "test-pkg", "test-pkg.spec")
	content, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Release: %autorelease")
}

func TestTryApplyReleaseCalculation_ExplicitStaticErrorsOnAutorelease(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	// Spec uses %autorelease, but config says static — should error.
	writeTestSpec(t, memFS, "test-pkg", "%autorelease")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationStatic,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `release.calculation = "autorelease"`)
}

// TestTryApplyReleaseCalculation_AutoPreservesAutoreleaseArgs verifies that the
// auto-detect path does not mutate a Release tag that already uses %autorelease
// with custom flags (-b, -p, -e, -s). The auto mode is the 90% case and must
// not silently strip author-specified arguments.
func TestTryApplyReleaseCalculation_AutoPreservesAutoreleaseArgs(t *testing.T) {
	cases := []string{
		"%autorelease -b 10",
		"%autorelease -p",
		"%autorelease -e %{?extraver}",
		"%autorelease -s %{date}git%{shortcommit}",
		"%autorelease -p -b 5 -e asan",
		"%{autorelease -e asan}",
		"%{?autorelease}",
	}

	for _, releaseVal := range cases {
		t.Run(releaseVal, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			memFS := afero.NewMemMapFs()
			preparer := newTestPreparer(memFS)

			writeTestSpec(t, memFS, "test-pkg", releaseVal)

			comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
				Release: projectconfig.ReleaseConfig{
					Calculation: projectconfig.ReleaseCalculationAuto,
				},
			})

			err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
			require.NoError(t, err)

			specPath := filepath.Join(testSourcesDir, "test-pkg", "test-pkg.spec")
			content, err := fileutils.ReadFile(memFS, specPath)
			require.NoError(t, err)
			assert.Contains(t, string(content), "Release: "+releaseVal+"\n",
				"auto mode must preserve existing %%autorelease form including args")
		})
	}
}

// TestTryApplyReleaseCalculation_ExplicitAutoreleasePreservesArgs verifies that
// the explicit 'autorelease' config — the documented escape hatch for weird
// packages — does not mutate the spec at all, regardless of what form the
// Release tag uses (%autorelease with args, macro wrappers, etc.).
func TestTryApplyReleaseCalculation_ExplicitAutoreleasePreservesArgs(t *testing.T) {
	cases := []string{
		"%autorelease -b 10",
		"%autorelease -p",
		"%autorelease -e %{?extraver}",
		"%{my_custom_autorelease_macro}",
		"%{?autorelease}%{!?autorelease:1%{?dist}}",
	}

	for _, releaseVal := range cases {
		t.Run(releaseVal, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			memFS := afero.NewMemMapFs()
			preparer := newTestPreparer(memFS)

			writeTestSpec(t, memFS, "test-pkg", releaseVal)

			specPath := filepath.Join(testSourcesDir, "test-pkg", "test-pkg.spec")
			origContent, err := fileutils.ReadFile(memFS, specPath)
			require.NoError(t, err)

			comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
				Release: projectconfig.ReleaseConfig{
					Calculation: projectconfig.ReleaseCalculationAutorelease,
				},
			})

			err = preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
			require.NoError(t, err)

			// The spec must be byte-identical: explicit autorelease config is
			// an escape hatch that means "trust the spec, do not touch".
			newContent, err := fileutils.ReadFile(memFS, specPath)
			require.NoError(t, err)
			assert.Equal(t, string(origContent), string(newContent),
				"explicit 'autorelease' config must not modify the spec file")
		})
	}
}
