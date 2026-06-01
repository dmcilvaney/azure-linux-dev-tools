// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// White-box test (package sources) so the unexported best-effort helper
// applyHistoricSpecOverlays can be exercised directly.
package sources

import (
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
	memfs "github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const historicTestSpec = `Name: foo
Version: 1.0.0
Release: 1%{?dist}
Summary: test
License: MIT

%description
Test.
`

func commitHistoricSpec(t *testing.T, repo *gogit.Repository, billyFS billy.Filesystem, content string) plumbing.Hash {
	t.Helper()

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	file, err := billyFS.Create("foo.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("foo.spec")
	require.NoError(t, err)

	hash, err := worktree.Commit("add spec", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(hash)
	require.NoError(t, err)

	return commit.TreeHash
}

func specInTree(t *testing.T, repo *gogit.Repository, treeHash plumbing.Hash) string {
	t.Helper()

	tree, err := repo.TreeObject(treeHash)
	require.NoError(t, err)

	entry := findTopLevelSpec(tree)
	require.NotNil(t, entry, "tree should contain a spec")

	blob, err := readBlob(repo, entry.Hash)
	require.NoError(t, err)

	return string(blob)
}

// TestApplyHistoricSpecOverlays_RewritesSpec verifies that a matching
// spec-search-replace overlay rewrites the spec blob in a new tree.
func TestApplyHistoricSpecOverlays_RewritesSpec(t *testing.T) {
	fs := memfs.New()

	repo, err := gogit.Init(memory.NewStorage(), fs)
	require.NoError(t, err)

	treeHash := commitHistoricSpec(t, repo, fs, historicTestSpec)

	overlays := []projectconfig.ComponentOverlay{{
		Type:        projectconfig.ComponentOverlaySearchAndReplaceInSpec,
		Regex:       `1\.0\.0`,
		Replacement: "2.0.0",
	}}

	newTree := applyHistoricSpecOverlays(repo, treeHash, overlays)
	assert.NotEqual(t, treeHash, newTree, "tree should change when overlay applies")
	assert.Contains(t, specInTree(t, repo, newTree), "Version: 2.0.0")
}

// TestApplyHistoricSpecOverlays_BestEffortLeavesTreeUnchanged verifies that when
// no overlay applies (the only overlay's regex matches nothing) the tree is left
// as-is.
func TestApplyHistoricSpecOverlays_BestEffortLeavesTreeUnchanged(t *testing.T) {
	fs := memfs.New()

	repo, err := gogit.Init(memory.NewStorage(), fs)
	require.NoError(t, err)

	treeHash := commitHistoricSpec(t, repo, fs, historicTestSpec)

	overlays := []projectconfig.ComponentOverlay{{
		Type:        projectconfig.ComponentOverlaySearchAndReplaceInSpec,
		Regex:       `this-text-does-not-exist`,
		Replacement: "x",
	}}

	newTree := applyHistoricSpecOverlays(repo, treeHash, overlays)
	assert.Equal(t, treeHash, newTree, "non-applying overlay must leave the tree unchanged")
}

// TestApplyHistoricSpecOverlays_PartialApplyKeepsMatching verifies that overlays
// are applied independently: when one overlay's anchor is absent (e.g. a
// `%setup` line phrased differently in an older upstream spec) it is skipped,
// but the version-setting overlay still applies. This is the core fix for
// historical version attribution — a failed structural overlay must not abandon
// the version overlay.
func TestApplyHistoricSpecOverlays_PartialApplyKeepsMatching(t *testing.T) {
	fs := memfs.New()

	repo, err := gogit.Init(memory.NewStorage(), fs)
	require.NoError(t, err)

	treeHash := commitHistoricSpec(t, repo, fs, historicTestSpec)

	overlays := []projectconfig.ComponentOverlay{
		{
			// Anchor absent in this (older) spec: must be skipped, not fatal.
			Type:        projectconfig.ComponentOverlaySearchAndReplaceInSpec,
			Regex:       `%setup -q -c`,
			Replacement: "%setup -q",
		},
		{
			// Version overlay: must still apply despite the failure above.
			Type:        projectconfig.ComponentOverlaySearchAndReplaceInSpec,
			Regex:       `1\.0\.0`,
			Replacement: "2.0.0",
		},
	}

	newTree := applyHistoricSpecOverlays(repo, treeHash, overlays)
	assert.NotEqual(t, treeHash, newTree, "tree should change because the version overlay still applies")
	assert.Contains(t, specInTree(t, repo, newTree), "Version: 2.0.0")
}

// TestApplyHistoricSpecOverlays_NoSpecOverlays verifies that an empty/non-spec
// overlay set is a no-op.
func TestApplyHistoricSpecOverlays_NoSpecOverlays(t *testing.T) {
	fs := memfs.New()

	repo, err := gogit.Init(memory.NewStorage(), fs)
	require.NoError(t, err)

	treeHash := commitHistoricSpec(t, repo, fs, historicTestSpec)

	newTree := applyHistoricSpecOverlays(repo, treeHash, nil)
	assert.Equal(t, treeHash, newTree)
}

// commitProjectFile writes relPath into the worktree and commits it, returning
// the commit hash (not the tree hash) so it can be used as a FingerprintChange
// hash.
func commitProjectFile(
	t *testing.T, repo *gogit.Repository, billyFS billy.Filesystem, relPath, content string,
) plumbing.Hash {
	t.Helper()

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	file, err := billyFS.Create(relPath)
	require.NoError(t, err)

	_, err = file.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add(relPath)
	require.NoError(t, err)

	hash, err := worktree.Commit("write "+relPath, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()},
	})
	require.NoError(t, err)

	return hash
}

// TestPopulateHistoricOverlays_PerCommitFidelity exercises the full
// populateHistoricOverlays wiring (hash parsing, per-commit config load, overlay
// resolution, dirty-entry skip) the way buildSyntheticCommits drives it. Each
// real commit must receive the overlays it carried at that commit; the synthetic
// ("dirty") entry must be left with nil overlays.
//
// This is the link most likely to silently no-op in the field: if per-commit
// resolution errored (caught by slog.Debug), every entry would keep nil overlays
// and the replay would do nothing.
func TestPopulateHistoricOverlays_PerCommitFidelity(t *testing.T) {
	bfs := memfs.New()

	repo, err := gogit.Init(memory.NewStorage(), bfs)
	require.NoError(t, err)

	hashA := commitProjectFile(t, repo, bfs, "azldev.toml", `
[components.foo]
[[components.foo.overlays]]
type = "spec-search-replace"
regex = "VERSION"
replacement = "2.0.0"
`)
	hashB := commitProjectFile(t, repo, bfs, "azldev.toml", `
[components.foo]
[[components.foo.overlays]]
type = "spec-search-replace"
regex = "VERSION"
replacement = "3.0.0"
`)

	changes := []FingerprintChange{
		{CommitMetadata: CommitMetadata{Hash: hashA.String()}},
		{CommitMetadata: CommitMetadata{Hash: hashB.String()}},
		{CommitMetadata: CommitMetadata{Hash: "dirty"}}, // synthetic entry: zero hash, skipped.
	}

	config := &projectconfig.ComponentConfig{
		Release: projectconfig.ReleaseConfig{ReplayHistoricalOverlays: true},
	}

	// projectRepoDir "/" + nil SourceConfigFile => referenceDir defaults to "/".
	populateHistoricOverlays(repo, "/", config, "foo", changes)

	require.Len(t, changes[0].Overlays, 1, "commit A must resolve its overlays")
	assert.Equal(t, "2.0.0", changes[0].Overlays[0].Replacement)
	require.Len(t, changes[1].Overlays, 1, "commit B must resolve its overlays")
	assert.Equal(t, "3.0.0", changes[1].Overlays[0].Replacement)
	assert.Nil(t, changes[2].Overlays, "synthetic/dirty entry must be skipped")
}
