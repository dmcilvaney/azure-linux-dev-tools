// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package instructions_test

import (
	"path/filepath"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx/opctx_test"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/testctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/instructions"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// expectedEmbeddedFiles returns the set of relative paths the embedded
// layout must always produce. Update this list whenever the embedded
// `content/` tree changes.
func expectedEmbeddedFiles() []string {
	return []string{
		"AGENTS.md",
		".github/copilot-instructions.md",
		".github/instructions/azldev.instructions.md",
		".github/plugins/azldev/plugin.json",
		".github/plugins/azldev/skills/azldev/SKILL.md",
		".github/plugins/azldev/agents/azldev.agent.md",
	}
}

func TestFiles(t *testing.T) {
	files, err := instructions.Files()
	require.NoError(t, err)
	assert.ElementsMatch(t, expectedEmbeddedFiles(), files,
		"embedded instruction file set drifted; update expectedEmbeddedFiles if intentional")
}

func TestProvision_RejectsRelativeDest(t *testing.T) {
	ctrl := gomock.NewController(t)
	dryRunnable := opctx_test.NewNoOpMockDryRunnable(ctrl)

	_, err := instructions.Provision(
		dryRunnable, afero.NewMemMapFs(),
		instructions.ScopeProject, "relative/path", instructions.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

func TestProvision_WritesAllFiles(t *testing.T) {
	ctrl := gomock.NewController(t)
	dryRunnable := opctx_test.NewNoOpMockDryRunnable(ctrl)
	memFS := afero.NewMemMapFs()

	const dest = "/dest"

	result, err := instructions.Provision(
		dryRunnable, memFS,
		instructions.ScopeProject, dest, instructions.Options{})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, dest, result.DestBase)
	assert.ElementsMatch(t, expectedEmbeddedFiles(), result.Written)
	assert.Empty(t, result.Skipped)

	// Each file should exist with non-empty content.
	for _, rel := range expectedEmbeddedFiles() {
		path := filepath.Join(dest, filepath.FromSlash(rel))
		exists, existsErr := fileutils.Exists(memFS, path)
		require.NoError(t, existsErr)
		assert.True(t, exists, "expected %s to exist", path)

		data, readErr := fileutils.ReadFile(memFS, path)
		require.NoError(t, readErr)
		assert.NotEmpty(t, data, "expected %s to be non-empty", path)
	}
}

func TestProvision_SkipsExisting(t *testing.T) {
	ctrl := gomock.NewController(t)
	dryRunnable := opctx_test.NewNoOpMockDryRunnable(ctrl)
	memFS := afero.NewMemMapFs()

	const dest = "/dest"

	// Pre-seed AGENTS.md with custom content.
	const customContent = "user-customized AGENTS.md content\n"

	require.NoError(t, fileutils.MkdirAll(memFS, dest))
	require.NoError(t, fileutils.WriteFile(memFS, filepath.Join(dest, "AGENTS.md"),
		[]byte(customContent), fileperms.PublicFile))

	result, err := instructions.Provision(
		dryRunnable, memFS,
		instructions.ScopeProject, dest, instructions.Options{Force: false})
	require.NoError(t, err)

	assert.Contains(t, result.Skipped, "AGENTS.md", "pre-existing file should be skipped")
	assert.NotContains(t, result.Written, "AGENTS.md")

	// Verify the custom content was preserved.
	data, err := fileutils.ReadFile(memFS, filepath.Join(dest, "AGENTS.md"))
	require.NoError(t, err)
	assert.Equal(t, customContent, string(data))
}

func TestProvision_ForceOverwrites(t *testing.T) {
	ctrl := gomock.NewController(t)
	dryRunnable := opctx_test.NewNoOpMockDryRunnable(ctrl)
	memFS := afero.NewMemMapFs()

	const dest = "/dest"

	require.NoError(t, fileutils.MkdirAll(memFS, dest))
	require.NoError(t, fileutils.WriteFile(memFS, filepath.Join(dest, "AGENTS.md"),
		[]byte("stale\n"), fileperms.PublicFile))

	result, err := instructions.Provision(
		dryRunnable, memFS,
		instructions.ScopeProject, dest, instructions.Options{Force: true})
	require.NoError(t, err)

	assert.ElementsMatch(t, expectedEmbeddedFiles(), result.Written)
	assert.Empty(t, result.Skipped)

	// Verify the file was overwritten.
	data, err := fileutils.ReadFile(memFS, filepath.Join(dest, "AGENTS.md"))
	require.NoError(t, err)
	assert.NotEqual(t, "stale\n", string(data))
}

func TestProvision_DryRunWritesNothing(t *testing.T) {
	ctrl := gomock.NewController(t)

	dryRunnable := opctx_test.NewMockDryRunnable(ctrl)
	dryRunnable.EXPECT().DryRun().AnyTimes().Return(true)

	memFS := afero.NewMemMapFs()

	const dest = "/dest"

	result, err := instructions.Provision(
		dryRunnable, memFS,
		instructions.ScopeProject, dest, instructions.Options{})
	require.NoError(t, err)

	// Dry-run reports the would-write set...
	assert.ElementsMatch(t, expectedEmbeddedFiles(), result.Written)

	// ...but no files actually exist.
	for _, rel := range expectedEmbeddedFiles() {
		exists, existsErr := fileutils.Exists(memFS, filepath.Join(dest, filepath.FromSlash(rel)))
		require.NoError(t, existsErr)
		assert.False(t, exists, "dry run must not write %s", rel)
	}
}

func TestUserDestBase_XDGConfigHome(t *testing.T) {
	osEnv := testctx.NewTestOSEnv()
	osEnv.SetEnv("XDG_CONFIG_HOME", "/abs/xdg")

	got, err := instructions.UserDestBase(osEnv)
	require.NoError(t, err)
	assert.Equal(t, filepath.FromSlash("/abs/xdg/azldev/instructions"), got)
}

func TestUserDestBase_HomeFallback(t *testing.T) {
	osEnv := testctx.NewTestOSEnv()
	osEnv.SetEnv("HOME", "/home/user")

	got, err := instructions.UserDestBase(osEnv)
	require.NoError(t, err)
	assert.Equal(t, filepath.FromSlash("/home/user/.config/azldev/instructions"), got)
}

func TestUserDestBase_NoneSet(t *testing.T) {
	osEnv := testctx.NewTestOSEnv()

	_, err := instructions.UserDestBase(osEnv)
	require.ErrorIs(t, err, instructions.ErrNoUserConfigDir)
}

func TestScope_String(t *testing.T) {
	assert.Equal(t, "project", instructions.ScopeProject.String())
	assert.Equal(t, "user", instructions.ScopeUser.String())
}
