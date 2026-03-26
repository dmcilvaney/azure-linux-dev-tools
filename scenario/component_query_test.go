// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build scenario

package scenario_tests

import (
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/scenario/internal/projecttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// We test running `azldev query component` to make sure that spec parsing works as expected.
func TestQueryingAComponent(t *testing.T) {
	t.Parallel()

	// Skip unless doing long tests
	if testing.Short() {
		t.Skip("skipping long test")
	}

	// Create a simple spec with a known name and version.
	spec := projecttest.NewSpec(
		projecttest.WithName("test-component"),
		projecttest.WithVersion("3.1.4.159"),
	)

	// Create a simple project with the spec, using test default configs for distro and mock configurations.
	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.UseTestDefaultConfigs(),
	)

	// Run the component query command with test default configs copied into the container.
	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "query", spec.GetName()},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	// Get the parsed JSON output.
	output := results.GetJSONResult()

	// There should just be one component in the output.
	require.Len(t, output, 1, "Expected one component in the output")
	componentOutput := output[0]

	// Check for component name.
	require.Contains(t, componentOutput, "component")
	assert.Equal(t, spec.GetName(), componentOutput["component"], "Expected component name to match")

	// Check for SRPM structure.
	require.Contains(t, componentOutput, "srpm")
	srpm, ok := componentOutput["srpm"].(map[string]interface{})
	require.True(t, ok, "srpm field is not a map")
	require.Contains(t, srpm, "name")
	assert.Equal(t, spec.GetName(), srpm["name"], "Expected SRPM name to match")

	// Check for version within SRPM.
	require.Contains(t, srpm, "version")
	versionMap, ok := srpm["version"].(map[string]interface{})
	require.True(t, ok, "version field is not a map")
	require.Contains(t, versionMap, "Version")
	assert.Equal(t, spec.GetVersion(), versionMap["Version"])

	// Check subpackages exist.
	require.Contains(t, componentOutput, "subpackages")
	subpackages, ok := componentOutput["subpackages"].([]interface{})
	require.True(t, ok, "subpackages field is not an array")
	require.NotEmpty(t, subpackages, "Expected at least one subpackage")

	// Validate SRPM NEVR.
	require.Contains(t, srpm, "nevr")
	assert.Equal(t, spec.GetExpectedNEVR(), srpm["nevr"], "Expected SRPM NEVR to match")

	// Validate subpackage NEVR.
	subpkg, ok := subpackages[0].(map[string]interface{})
	require.True(t, ok, "subpackage is not a map")
	require.Contains(t, subpkg, "nevr")
	assert.Equal(t, spec.GetExpectedNEVR(), subpkg["nevr"], "Expected subpackage NEVR to match")
}

// Test that component query returns correct NEVRs for a multi-subpackage component.
func TestQueryingMultiSubpackageComponent(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	spec := projecttest.NewSpec(
		projecttest.WithName("multi-pkg"),
		projecttest.WithVersion("2.0.0"),
		projecttest.WithRelease("1.rel"),
		projecttest.WithSubpackage("devel"),
		projecttest.WithSubpackage("libs"),
	)

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.UseTestDefaultConfigs(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "query", spec.GetName()},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	output := results.GetJSONResult()
	require.Len(t, output, 1, "Expected one component in the output")

	comp := output[0]

	// Validate SRPM NEVR.
	srpm, ok := comp["srpm"].(map[string]interface{})
	require.True(t, ok, "srpm field is not a map")
	assert.Equal(t, spec.GetExpectedNEVR(), srpm["nevr"], "SRPM NEVR mismatch")

	// Validate subpackages: base + devel + libs = 3.
	subpackages, ok := comp["subpackages"].([]interface{})
	require.True(t, ok, "subpackages field is not an array")
	require.Len(t, subpackages, 3, "Expected 3 subpackages: base, devel, libs")

	// Build a map of subpackage name → NEVR for validation.
	subpkgNEVRs := make(map[string]string)

	for _, sp := range subpackages {
		spMap, ok := sp.(map[string]interface{})
		require.True(t, ok, "subpackage entry is not a map")

		name, ok := spMap["name"].(string)
		require.True(t, ok, "subpackage name is not a string")

		nevr, ok := spMap["nevr"].(string)
		require.True(t, ok, "subpackage nevr is not a string")

		subpkgNEVRs[name] = nevr
	}

	assert.Equal(t, spec.GetExpectedNEVR(), subpkgNEVRs["multi-pkg"],
		"Base package NEVR mismatch")
	assert.Equal(t, spec.GetExpectedSubpackageNEVR("devel"), subpkgNEVRs["multi-pkg-devel"],
		"Devel package NEVR mismatch")
	assert.Equal(t, spec.GetExpectedSubpackageNEVR("libs"), subpkgNEVRs["multi-pkg-libs"],
		"Libs package NEVR mismatch")
}

// Test that component query returns correct NEVRs when epoch is non-zero.
func TestQueryingComponentWithEpoch(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	spec := projecttest.NewSpec(
		projecttest.WithName("epoch-pkg"),
		projecttest.WithVersion("1.0.0"),
		projecttest.WithRelease("1.rel"),
		projecttest.WithEpoch("3"),
	)

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.UseTestDefaultConfigs(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "query", spec.GetName()},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	output := results.GetJSONResult()
	require.Len(t, output, 1, "Expected one component in the output")

	comp := output[0]

	// Validate SRPM NEVR includes epoch.
	srpm, ok := comp["srpm"].(map[string]interface{})
	require.True(t, ok, "srpm field is not a map")
	assert.Equal(t, spec.GetExpectedNEVR(), srpm["nevr"], "SRPM NEVR should include epoch")

	// Validate subpackage NEVR includes epoch.
	subpackages, ok := comp["subpackages"].([]interface{})
	require.True(t, ok, "subpackages field is not an array")
	require.Len(t, subpackages, 1)

	subpkg, ok := subpackages[0].(map[string]interface{})
	require.True(t, ok, "subpackage is not a map")
	assert.Equal(t, spec.GetExpectedNEVR(), subpkg["nevr"], "Subpackage NEVR should include epoch")
}
