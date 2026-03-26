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
}
