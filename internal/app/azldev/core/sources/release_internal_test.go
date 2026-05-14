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

func TestTryApplyReleaseCalculation_AutoSkipsAutorelease(t *testing.T) {
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

func TestTryApplyReleaseCalculation_AutoFlipsStaticToAutorelease(t *testing.T) {
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

	// Verify the spec was flipped to %autorelease.
	specPath := filepath.Join(testSourcesDir, "test-pkg", "test-pkg.spec")
	content, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Release: %autorelease")
}

func TestTryApplyReleaseCalculation_AutoFlipsNonStandard(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	// Non-standard release (e.g. macro-based) — auto mode flips it to %autorelease.
	writeTestSpec(t, memFS, "kernel", "%{pkg_release}")

	comp := mockComponent(ctrl, "kernel", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAuto,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "kernel"))
	require.NoError(t, err)

	specPath := filepath.Join(testSourcesDir, "kernel", "kernel.spec")
	content, err := fileutils.ReadFile(memFS, specPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Release: %autorelease")
}

func TestTryApplyReleaseCalculation_NonStandardSucceedsWithManual(t *testing.T) {
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

func TestTryApplyReleaseCalculation_ExplicitAutoreleaseValidates(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "gvisor", "%autorelease")

	comp := mockComponent(ctrl, "gvisor", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAutorelease,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "gvisor"))
	require.NoError(t, err)
}

func TestTryApplyReleaseCalculation_ExplicitAutoreleaseErrorsOnStatic(t *testing.T) {
	ctrl := gomock.NewController(t)
	memFS := afero.NewMemMapFs()
	preparer := newTestPreparer(memFS)

	writeTestSpec(t, memFS, "test-pkg", "1%{?dist}")

	comp := mockComponent(ctrl, "test-pkg", &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{
			Calculation: projectconfig.ReleaseCalculationAutorelease,
		},
	})

	err := preparer.tryApplyReleaseCalculation(comp, filepath.Join(testSourcesDir, "test-pkg"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not %autorelease")
}

func TestTryApplyReleaseCalculation_ExplicitStaticFlips(t *testing.T) {
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

	// Verify the spec was flipped to %autorelease.
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
