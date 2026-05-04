// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component_test

import (
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	componentcmds "github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/component"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testComponentName = "curl"

// setupMockGitWithCounter configures mock git that tracks all git command invocations.
// Returns a pointer to the atomic counter so tests can assert network usage.
// Any git command (clone, rev-list, rev-parse) counts as "network" since the
// Fedora source provider needs network access for all of them.
func setupMockGitWithCounter(env *testutils.TestEnv, commitHash string) *atomic.Int32 {
	var gitCalls atomic.Int32

	env.CmdFactory.RegisterCommandInSearchPath("git")

	env.CmdFactory.RunHandler = func(cmd *exec.Cmd) error {
		gitCalls.Add(1)

		args := cmd.Args

		for _, arg := range args {
			if arg == "clone" {
				destDir := args[len(args)-1]
				_ = fileutils.MkdirAll(env.TestFS, destDir)

				return nil
			}

			if arg == "checkout" {
				return nil
			}
		}

		return nil
	}

	env.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		gitCalls.Add(1)

		if strings.Contains(strings.Join(cmd.Args, " "), "rev-parse") {
			return commitHash, nil
		}

		if strings.Contains(strings.Join(cmd.Args, " "), "rev-list") {
			return commitHash, nil
		}

		return "", nil
	}

	return &gitCalls
}

// allComponentsFilter returns a filter for all components with lock validation skipped.
func allComponentsFilter() *componentcmds.UpdateComponentOptions {
	return &componentcmds.UpdateComponentOptions{
		ComponentFilter: components.ComponentFilter{IncludeAllComponents: true},
	}
}

// initialUpdate runs the first update to establish lock files. Returns the
// initial fingerprint and resolution hash for the named component.
func initialUpdate(
	t *testing.T, env *testutils.TestEnv,
) (fingerprint, resHash string) {
	t.Helper()

	require.NoError(t, fileutils.MkdirAll(env.TestFS, testLockDir))

	results, err := componentcmds.UpdateComponents(env.Env, allComponentsFilter())
	require.NoError(t, err)
	require.NotEmpty(t, results, "initial update should produce results")

	store := lockfile.NewStore(env.TestFS, testLockDir)

	lock, lockErr := store.Get(testComponentName)
	require.NoError(t, lockErr)
	require.NotEmpty(t, lock.InputFingerprint, "initial lock should have fingerprint")
	require.NotEmpty(t, lock.ResolutionInputHash, "initial lock should have resolution hash")

	return lock.InputFingerprint, lock.ResolutionInputHash
}

// TestFreshness_NothingChanged_SkipsNetwork verifies Case 1: when nothing
// changed between updates, no git clones happen and the lock is untouched.
func TestFreshness_NothingChanged_SkipsNetwork(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "aabbccdd11223344"

	gitCalls := setupMockGitWithCounter(env, commit)
	addUpstreamComponent(env, testComponentName)
	setDistroSnapshot(env, "2025-01-01T00:00:00Z")

	fpBefore, resHashBefore := initialUpdate(t, env)

	// Reset clone counter after initial update.
	initialClones := gitCalls.Load()
	require.Positive(t, initialClones, "initial update must do at least one clone")

	gitCalls.Store(0)

	// Second update — nothing changed.
	results, err := componentcmds.UpdateComponents(env.Env, allComponentsFilter())
	require.NoError(t, err)

	// No git clones should have happened.
	assert.Equal(t, int32(0), gitCalls.Load(),
		"no git calls expected when nothing changed")

	// No results in display (up-to-date filtered out).
	for _, r := range results {
		if r.Component == testComponentName {
			assert.False(t, r.Changed, "curl should not be changed")
			assert.False(t, r.Skipped, "curl should not be skipped")
		}
	}

	// Lock file should be identical.
	store := lockfile.NewStore(env.TestFS, testLockDir)

	lock, lockErr := store.Get("curl")
	require.NoError(t, lockErr)
	assert.Equal(t, fpBefore, lock.InputFingerprint, "fingerprint should be unchanged")
	assert.Equal(t, resHashBefore, lock.ResolutionInputHash, "resolution hash should be unchanged")
	assert.Equal(t, commit, lock.UpstreamCommit, "commit should be unchanged")
}

// setDistroSnapshot updates the test distro version's default-component-config
// snapshot. This mirrors how real projects set snapshots in distro.toml.
func setDistroSnapshot(env *testutils.TestEnv, snapshot string) {
	distro := env.Config.Distros["test-distro"]

	version := distro.Versions["1.0"]
	version.DefaultComponentConfig.Spec.UpstreamDistro.Snapshot = snapshot
	distro.Versions["1.0"] = version

	env.Config.Distros["test-distro"] = distro
}

// TestFreshness_SnapshotChanged_SameCommit_UsesNetwork verifies Case 2: when
// the snapshot changes but the resolved commit is the same, the system
// re-resolves (network), updates the resolution hash, but the fingerprint
// stays the same (commit unchanged → build output unchanged).
func TestFreshness_SnapshotChanged_SameCommit_UsesNetwork(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "aabbccdd11223344"

	gitCalls := setupMockGitWithCounter(env, commit)
	addUpstreamComponent(env, testComponentName)
	setDistroSnapshot(env, "2025-01-01T00:00:00Z")

	fpBefore, resHashBefore := initialUpdate(t, env)

	gitCalls.Store(0)

	// Bump snapshot at the distro level — resolution inputs change.
	setDistroSnapshot(env, "2026-06-15T00:00:00Z")

	// Second update — should re-resolve (network).
	results, err := componentcmds.UpdateComponents(env.Env, allComponentsFilter())
	require.NoError(t, err)

	assert.Positive(t, gitCalls.Load(),
		"git calls expected when snapshot changed")

	// Fingerprint should be UNCHANGED (same commit, snapshot excluded from fingerprint).
	store := lockfile.NewStore(env.TestFS, testLockDir)

	lock, lockErr := store.Get("curl")
	require.NoError(t, lockErr)
	assert.Equal(t, fpBefore, lock.InputFingerprint,
		"fingerprint should be unchanged (same commit, snapshot excluded)")
	assert.Equal(t, commit, lock.UpstreamCommit,
		"commit should be the same (mock returns same hash)")

	// Resolution hash should be CHANGED (snapshot is a resolution input).
	assert.NotEqual(t, resHashBefore, lock.ResolutionInputHash,
		"resolution hash should change after snapshot bump")

	// Component should NOT be in display results as "changed" (fingerprint same).
	for _, r := range results {
		if r.Component == testComponentName {
			assert.False(t, r.Changed,
				"curl should not be marked changed (fingerprint unchanged)")
		}
	}
}

// TestFreshness_OverlayChanged_SkipsNetwork verifies Case 3: when only build
// inputs change (overlay edit) but resolution inputs are unchanged, the system
// reuses the locked commit (no network) and updates the fingerprint.
func TestFreshness_OverlayChanged_SkipsNetwork(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const commit = "aabbccdd11223344"

	gitCalls := setupMockGitWithCounter(env, commit)
	addUpstreamComponent(env, testComponentName)
	setDistroSnapshot(env, "2025-01-01T00:00:00Z")

	fpBefore, resHashBefore := initialUpdate(t, env)

	gitCalls.Store(0)

	// Add a build option — changes fingerprint but not resolution inputs.
	modifiedConfig := env.Config.Components["curl"]
	modifiedConfig.Build.With = []string{"ssl"}
	env.Config.Components["curl"] = modifiedConfig

	// Second update — should NOT re-resolve.
	results, err := componentcmds.UpdateComponents(env.Env, allComponentsFilter())
	require.NoError(t, err)

	assert.Equal(t, int32(0), gitCalls.Load(),
		"no git calls expected when only build inputs changed")

	// Fingerprint should be CHANGED (build input added).
	store := lockfile.NewStore(env.TestFS, testLockDir)

	lock, lockErr := store.Get("curl")
	require.NoError(t, lockErr)
	assert.NotEqual(t, fpBefore, lock.InputFingerprint,
		"fingerprint should change after build input change")
	assert.Equal(t, commit, lock.UpstreamCommit,
		"commit should be unchanged (reused from lock)")

	// Resolution hash should be UNCHANGED.
	assert.Equal(t, resHashBefore, lock.ResolutionInputHash,
		"resolution hash should be unchanged (no resolution input changed)")

	// Component SHOULD be in display results as "changed".
	foundChanged := false

	for _, r := range results {
		if r.Component == testComponentName && r.Changed {
			foundChanged = true
		}
	}

	assert.True(t, foundChanged,
		"curl should appear as changed in results (fingerprint differs)")
}

// TestFreshness_SnapshotChanged_DifferentCommit verifies Case 4: when
// snapshot changes AND the resolved commit is different, both the
// resolution hash and fingerprint should change.
func TestFreshness_SnapshotChanged_DifferentCommit(t *testing.T) {
	env := testutils.NewTestEnv(t)

	const (
		initialCommit = "aabbccdd11223344"
		newCommit     = "eeff00112233aabb"
	)

	gitCalls := setupMockGitWithCounter(env, initialCommit)
	addUpstreamComponent(env, testComponentName)
	setDistroSnapshot(env, "2025-01-01T00:00:00Z")

	fpBefore, resHashBefore := initialUpdate(t, env)

	gitCalls.Store(0)

	// Bump snapshot AND change the commit the mock returns.
	gitCalls = setupMockGitWithCounter(env, newCommit)
	setDistroSnapshot(env, "2026-06-15T00:00:00Z")

	results, err := componentcmds.UpdateComponents(env.Env, allComponentsFilter())
	require.NoError(t, err)

	assert.Positive(t, gitCalls.Load(),
		"git calls expected when snapshot changed")

	store := lockfile.NewStore(env.TestFS, testLockDir)

	lock, lockErr := store.Get("curl")
	require.NoError(t, lockErr)

	// Everything should change.
	assert.Equal(t, newCommit, lock.UpstreamCommit,
		"commit should be the new one")
	assert.NotEqual(t, fpBefore, lock.InputFingerprint,
		"fingerprint should change (different commit)")
	assert.NotEqual(t, resHashBefore, lock.ResolutionInputHash,
		"resolution hash should change (snapshot bumped)")

	// Component should be marked changed.
	foundChanged := false

	for _, r := range results {
		if r.Component == testComponentName && r.Changed {
			foundChanged = true
		}
	}

	assert.True(t, foundChanged,
		"curl should appear as changed (new commit → new fingerprint)")
}
