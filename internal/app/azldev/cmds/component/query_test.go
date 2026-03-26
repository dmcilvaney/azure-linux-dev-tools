// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/component"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/mock"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewComponentQueryCommand(t *testing.T) {
	cmd := component.NewComponentQueryCommand()
	require.NotNil(t, cmd)
	assert.Equal(t, "query", cmd.Use)

	// Verify --local-only flag is registered.
	localOnlyFlag := cmd.Flags().Lookup("local-only")
	require.NotNil(t, localOnlyFlag, "--local-only flag should be registered")
	assert.Equal(t, "false", localOnlyFlag.DefValue)
}

func TestComponentQueryCmd_NoMatch(t *testing.T) {
	const testComponentName = "test-component"

	testEnv := testutils.NewTestEnv(t)

	cmd := component.NewComponentQueryCommand()
	cmd.SetArgs([]string{testComponentName})

	err := cmd.ExecuteContext(testEnv.Env)

	// We expect an error because we haven't set up any components.
	require.Error(t, err)
}

func TestQueryComponents_OneComponent(t *testing.T) {
	const (
		testComponentName = "test-component"
		testSpecPath      = "/path/to/spec"
	)

	testEnv := testutils.NewTestEnv(t)
	testEnv.Config.Components[testComponentName] = projectconfig.ComponentConfig{
		Name: testComponentName,
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeLocal,
			Path:       testSpecPath,
		},
	}

	// Pretend mock is present.
	testEnv.CmdFactory.RegisterCommandInSearchPath(mock.MockBinary)

	// Mock the rpmspec command to return valid output.
	// NOTE: This takes a dependency on knowing how rpmspec gets invoked.
	// The first call uses --srpm for SRPM info; the second call omits it for binary subpackages.
	testEnv.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		for _, arg := range cmd.Args {
			if strings.Contains(arg, "--srpm") {
				return "name=test-component\nepoch=0\nversion=1.0.0\nrelease=1.azl3\n", nil
			}
		}

		return "pkg_name=test-component\npkg_epoch=0\npkg_version=1.0.0\npkg_release=1.azl3\n---\n", nil
	}

	options := component.QueryComponentsOptions{
		ComponentFilter: components.ComponentFilter{
			ComponentNamePatterns: []string{testComponentName},
		},
	}

	// Simulate the spec file existing.
	err := fileutils.WriteFile(testEnv.FS(), testSpecPath, []byte("test spec content"), fileperms.PublicFile)
	require.NoError(t, err)

	results, err := component.QueryComponents(testEnv.Env, &options)
	require.NoError(t, err)
	require.Len(t, results, 1)

	result := results[0]
	assert.Equal(t, testComponentName, result.ComponentName)
	assert.Equal(t, testComponentName, result.Name)
	assert.Equal(t, "test-component-1.0.0-1.azl3", result.NEVR)
	require.Len(t, result.Subpackages, 1)
	assert.Equal(t, testComponentName, result.Subpackages[0].Name)
}

func TestQueryComponents_LocalOnlyFailsWithoutFallback(t *testing.T) {
	const (
		testComponentName = "test-component"
		testSpecPath      = "/path/to/spec"
	)

	testEnv := testutils.NewTestEnv(t)
	testEnv.Config.Components[testComponentName] = projectconfig.ComponentConfig{
		Name: testComponentName,
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeLocal,
			Path:       testSpecPath,
		},
	}

	// Pretend mock is present.
	testEnv.CmdFactory.RegisterCommandInSearchPath(mock.MockBinary)

	// Mock the rpmspec command to return an error (simulating unresolvable macros).
	testEnv.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		return "error: failed to resolve macros", errors.New("rpmspec failed")
	}

	options := component.QueryComponentsOptions{
		ComponentFilter: components.ComponentFilter{
			ComponentNamePatterns: []string{testComponentName},
		},
		LocalOnly: true,
	}

	// Simulate the spec file existing.
	err := fileutils.WriteFile(testEnv.FS(), testSpecPath, []byte("test spec content"), fileperms.PublicFile)
	require.NoError(t, err)

	// With --local-only, the query should fail without attempting source fallback.
	_, err = component.QueryComponents(testEnv.Env, &options)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse spec for component")
}
