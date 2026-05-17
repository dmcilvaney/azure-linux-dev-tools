// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources_test

import (
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	memfs "github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitSpec creates a commit with a single spec file and returns the commit hash.
func commitSpec(t *testing.T, repo *gogit.Repository, memFS billy.Filesystem, specContent string) plumbing.Hash {
	t.Helper()

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte(specContent))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	hash, err := worktree.Commit("test commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	return hash
}

// treeHashOf returns the tree hash of a commit.
func treeHashOf(t *testing.T, repo *gogit.Repository, commitHash plumbing.Hash) plumbing.Hash {
	t.Helper()

	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	return commit.TreeHash
}

// readSpecFromTree reads the spec file content from a tree.
func readSpecFromTree(t *testing.T, repo *gogit.Repository, treeHash plumbing.Hash) string {
	t.Helper()

	tree, err := repo.TreeObject(treeHash)
	require.NoError(t, err)

	for _, entry := range tree.Entries {
		if entry.Name == "package.spec" {
			blob, blobErr := repo.BlobObject(entry.Hash)
			require.NoError(t, blobErr)

			reader, readerErr := blob.Reader()
			require.NoError(t, readerErr)

			defer reader.Close()

			buf := make([]byte, blob.Size)
			_, readErr := reader.Read(buf)
			require.NoError(t, readErr)

			return string(buf)
		}
	}

	t.Fatal("package.spec not found in tree")

	return ""
}

func TestContract_Materialize_FlipsBothChangelogAndRelease(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Entry.
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)
	assert.NotEqual(t, originalTree, newTree, "tree should change after flipping")

	spec := readSpecFromTree(t, repo, newTree)
	assert.Contains(t, spec, "Release: %autorelease")
	assert.Contains(t, spec, "%autochangelog")
	assert.NotContains(t, spec, "Release: 5%{?dist}")
}

func TestContract_Materialize_NoChangelogSection_StillFlipsRelease(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Release: 3%{?dist}
Summary: test
License: MIT

%description
Test.
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)
	assert.NotEqual(t, originalTree, newTree, "tree should change — Release flipped")

	spec := readSpecFromTree(t, repo, newTree)
	assert.Contains(t, spec, "Release: %autorelease")
}

func TestContract_Materialize_NoReleaseTag_StillFlipsChangelog(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Summary: test
License: MIT

%description
Test.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Entry.
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)
	assert.NotEqual(t, originalTree, newTree, "tree should change — changelog flipped")

	spec := readSpecFromTree(t, repo, newTree)
	assert.Contains(t, spec, "%autochangelog")
}

func TestContract_Materialize_AlreadySatisfied_ReturnsSameHash(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Release: %autorelease
Summary: test
License: MIT

%description
Test.

%changelog
%autochangelog
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)
	assert.Equal(t, originalTree, newTree, "already-satisfied tree must return same hash")
}

func TestContract_Materialize_PreservesNonSpecEntries(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Create spec + extra file.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: foo\nVersion: 1.0\nRelease: 1\n\n%description\nTest.\n\n%changelog\n* Entry\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	patchFile, err := memFS.Create("fix.patch")
	require.NoError(t, err)

	_, err = patchFile.Write([]byte("--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n"))
	require.NoError(t, err)
	require.NoError(t, patchFile.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	_, err = worktree.Add("fix.patch")
	require.NoError(t, err)

	commitHash, err := worktree.Commit("test", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t", When: time.Unix(0, 0).UTC()},
	})
	require.NoError(t, err)

	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)

	// Check that fix.patch is preserved byte-for-byte.
	tree, err := repo.TreeObject(newTree)
	require.NoError(t, err)

	var patchHash plumbing.Hash

	for _, entry := range tree.Entries {
		if entry.Name == "fix.patch" {
			patchHash = entry.Hash
		}
	}

	require.NotEqual(t, plumbing.ZeroHash, patchHash, "fix.patch must exist in output tree")

	origTree, err := repo.TreeObject(originalTree)
	require.NoError(t, err)

	for _, entry := range origTree.Entries {
		if entry.Name == "fix.patch" {
			assert.Equal(t, entry.Hash, patchHash, "fix.patch must be byte-for-byte identical")
		}
	}
}

func TestContract_Materialize_InjectsSidecar(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Release: 1
Summary: test
License: MIT

%description
Test.

%changelog
* Entry
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	// Create a sidecar blob.
	sidecarData := []byte("* Old changelog entry\n")
	sidecarObj := repo.Storer.NewEncodedObject()
	sidecarObj.SetType(plumbing.BlobObject)
	sidecarObj.SetSize(int64(len(sidecarData)))

	w, err := sidecarObj.Writer()
	require.NoError(t, err)

	_, err = w.Write(sidecarData)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	sidecarHash, err := repo.Storer.SetEncodedObject(sidecarObj)
	require.NoError(t, err)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true, SidecarBlob: sidecarHash}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)

	// Verify sidecar exists in the new tree.
	tree, err := repo.TreeObject(newTree)
	require.NoError(t, err)

	found := false

	for _, entry := range tree.Entries {
		if entry.Name == "changelog" {
			found = true

			assert.Equal(t, sidecarHash, entry.Hash, "sidecar blob hash must match")
		}
	}

	assert.True(t, found, "changelog sidecar must be present in tree")
}

func TestContract_CommitMessage_WithSkipChangelog(t *testing.T) {
	contract := sources.Contract{FlipRelease: true, FlipChangelog: true, SkipChangelog: true}
	msg := contract.CommitMessage("bump abc1234 (1/5)")
	assert.Contains(t, msg, "bump abc1234 (1/5)")
	assert.Contains(t, msg, "\n\n"+sources.SkipChangelogMarker)
}

func TestContract_CommitMessage_WithoutSkipChangelog(t *testing.T) {
	contract := sources.Contract{FlipRelease: true, FlipChangelog: true, SkipChangelog: false}
	msg := contract.CommitMessage("normal commit")
	assert.Equal(t, "normal commit", msg)
	assert.NotContains(t, msg, sources.SkipChangelogMarker)
}

func TestContract_Materialize_NoSpec_ReturnsSameHash(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Commit with no spec file.
	f, err := memFS.Create("README.md")
	require.NoError(t, err)

	_, err = f.Write([]byte("# Hello\n"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = worktree.Add("README.md")
	require.NoError(t, err)

	commitHash, err := worktree.Commit("no spec", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t", When: time.Unix(0, 0).UTC()},
	})
	require.NoError(t, err)

	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)
	assert.Equal(t, originalTree, newTree, "tree with no spec should be returned unchanged")
}

func TestContract_Materialize_NoFlips_PreservesSpec(t *testing.T) {
	// Regression test: when both FlipRelease and FlipChangelog are false (manual
	// release + manual changelog component), the spec must NOT be rewritten.
	// Injecting %autorelease / %autochangelog into a manual spec triggers
	// rpmautospec to walk the entire upstream history during process-distgit.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: kernel
Version: 6.18.29
Release: %{pkg_release}
Summary: test
License: GPL

%description
Test.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 6.18.29-1
- Manual entry.
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	// Both flips disabled — the manual-release + manual-changelog case.
	contract := sources.Contract{FlipRelease: false, FlipChangelog: false}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)
	assert.Equal(t, originalTree, newTree,
		"tree must be unchanged when both flips are disabled")

	specOut := readSpecFromTree(t, repo, newTree)
	assert.Contains(t, specOut, "Release: %{pkg_release}",
		"original Release tag must be preserved")
	assert.NotContains(t, specOut, "%autorelease",
		"must not inject %autorelease into manual-release spec")
	assert.NotContains(t, specOut, "%autochangelog",
		"must not inject %autochangelog into manual-changelog spec")
}

func TestContract_Materialize_OnlyFlipRelease(t *testing.T) {
	// Release flip enabled, changelog flip disabled — verifies independence.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Entry.
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: true, FlipChangelog: false}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)

	specOut := readSpecFromTree(t, repo, newTree)
	assert.Contains(t, specOut, "Release: %autorelease", "Release must be flipped")
	assert.NotContains(t, specOut, "%autochangelog", "%changelog must NOT be flipped")
	assert.Contains(t, specOut, "- Entry.", "original changelog entries must remain")
}

func TestContract_Materialize_OnlyFlipChangelog(t *testing.T) {
	// Changelog flip enabled, release flip disabled — verifies independence.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	specContent := `Name: foo
Version: 1.0
Release: 5%{?dist}
Summary: test
License: MIT

%description
Test.

%changelog
* Mon Jan 01 2024 Someone <s@example.com> - 1.0-1
- Entry.
`

	commitHash := commitSpec(t, repo, memFS, specContent)
	originalTree := treeHashOf(t, repo, commitHash)

	contract := sources.Contract{FlipRelease: false, FlipChangelog: true}

	newTree, err := contract.Materialize(repo, originalTree)
	require.NoError(t, err)

	specOut := readSpecFromTree(t, repo, newTree)
	assert.Contains(t, specOut, "Release: 5%{?dist}", "Release must NOT be flipped")
	assert.NotContains(t, specOut, "%autorelease", "must not inject %autorelease")
	assert.Contains(t, specOut, "%autochangelog", "%changelog body must be flipped")
}
