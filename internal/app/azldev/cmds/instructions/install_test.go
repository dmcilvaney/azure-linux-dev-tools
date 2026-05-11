// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package instructions_test

import (
	"path/filepath"
	"testing"

	cmdpkg "github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/instructions"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/testctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInstallCmd(t *testing.T) {
	cmd := cmdpkg.NewInstallCmd()
	require.NotNil(t, cmd)

	assert.Equal(t, "install", cmd.Name())
	assert.NotNil(t, cmd.Flag("user"))
	assert.NotNil(t, cmd.Flag("force"))
	assert.NotNil(t, cmd.RunE)
}

func TestInstallCmd_Project(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const projectDir = "/myproj"
	require.NoError(t, fileutils.MkdirAll(env.FS(), projectDir))
	require.NoError(t, env.TestOSEnv.Chdir(projectDir))

	cmd := cmdpkg.NewInstallCmd()
	cmd.SetContext(env.Env)
	cmd.SetArgs([]string{})

	require.NoError(t, cmd.Execute())

	// The well-known files should now exist under the project dir.
	for _, rel := range []string{
		"AGENTS.md",
		filepath.FromSlash(".github/copilot-instructions.md"),
		filepath.FromSlash(".github/instructions/azldev.instructions.md"),
	} {
		exists, err := fileutils.Exists(env.FS(), filepath.Join(projectDir, rel))
		require.NoError(t, err)
		assert.True(t, exists, "expected %s to be installed", rel)
	}
}

func TestInstallCmd_User(t *testing.T) {
	env := testutils.NewTestEnv(t)

	// Configure a fake user config home.
	const xdg = "/xdg-config"

	osEnv, ok := env.TestOSEnv.(*testctx.TestOSEnv)
	require.True(t, ok, "expected TestOSEnv to be a *testctx.TestOSEnv")
	osEnv.SetEnv("XDG_CONFIG_HOME", xdg)

	cmd := cmdpkg.NewInstallCmd()
	cmd.SetContext(env.Env)
	cmd.SetArgs([]string{"--user"})

	require.NoError(t, cmd.Execute())

	// Files should land under the user's config dir.
	expected := filepath.Join(xdg, "azldev", "instructions", "AGENTS.md")
	exists, err := fileutils.Exists(env.FS(), expected)
	require.NoError(t, err)
	assert.True(t, exists, "expected %s to exist", expected)
}

func TestInstallCmd_SkipsExistingWithoutForce(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const projectDir = "/myproj"
	require.NoError(t, fileutils.MkdirAll(env.FS(), projectDir))
	require.NoError(t, env.TestOSEnv.Chdir(projectDir))

	const customContent = "kept by user\n"

	agentsPath := filepath.Join(projectDir, "AGENTS.md")
	require.NoError(t, fileutils.WriteFile(env.FS(), agentsPath, []byte(customContent), fileperms.PublicFile))

	cmd := cmdpkg.NewInstallCmd()
	cmd.SetContext(env.Env)
	cmd.SetArgs([]string{})

	require.NoError(t, cmd.Execute())

	got, err := fileutils.ReadFile(env.FS(), agentsPath)
	require.NoError(t, err)
	assert.Equal(t, customContent, string(got),
		"AGENTS.md should be left untouched without --force")
}
