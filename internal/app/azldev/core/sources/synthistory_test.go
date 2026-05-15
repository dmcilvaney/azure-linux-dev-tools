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
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitInterleavedHistory_AllOnTop(t *testing.T) {
	// When all fingerprint changes reference the latest upstream commit,
	// all synthetic commits should be appended on top.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Create an upstream commit.
	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte("Name: package\nVersion: 1.0\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstreamCommit, err := worktree.Commit("upstream: initial", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Simulate overlay modification.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\nVersion: 1.0\n# overlays applied\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	upstreamHash := upstreamCommit.String()

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "abc123",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2025, 1, 10, 10, 0, 0, 0, time.UTC).Unix(),
				Message:     "Apply patch fix",
			},
			UpstreamCommit: upstreamHash,
		},
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "def456",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2025, 2, 20, 14, 0, 0, 0, time.UTC).Unix(),
				Message:     "Bump release",
			},
			UpstreamCommit: upstreamHash,
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, "", nil)
	require.NoError(t, err)

	// Verify the commit log: upstream + 2 synthetic = 3 commits.
	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	require.Len(t, logCommits, 3, "should have upstream + 2 synthetic commits")

	// Most recent commit (Bob's) — this is the last synthetic commit.
	assert.Contains(t, logCommits[0].Message, "Bump release")
	assert.Equal(t, "Bob", logCommits[0].Author.Name)

	// Second commit (Alice's).
	assert.Contains(t, logCommits[1].Message, "Apply patch fix")
	assert.Equal(t, "Alice", logCommits[1].Author.Name)

	// Original upstream commit.
	assert.Equal(t, "upstream: initial", logCommits[2].Message)
}

func TestCommitInterleavedHistory_Interleaved(t *testing.T) {
	// Two upstream commits, one synthetic change for the first (older) upstream
	// commit and one for the second (latest). The interleaved commit should
	// appear between the two upstream commits.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Upstream commit 1.
	file1, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file1.Write([]byte("Name: package\nVersion: 1.0\n"))
	require.NoError(t, err)
	require.NoError(t, file1.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream1, err := worktree.Commit("upstream: v1.0", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Upstream commit 2.
	file2, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file2.Write([]byte("Name: package\nVersion: 2.0\n"))
	require.NoError(t, err)
	require.NoError(t, file2.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream2, err := worktree.Commit("upstream: v2.0", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Simulate overlay modification in working tree.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\nVersion: 2.0\n# overlays\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-aaa",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Fix for v1.0",
			},
			UpstreamCommit: upstream1.String(), // references older upstream.
		},
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-bbb",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Fix for v2.0",
			},
			UpstreamCommit: upstream2.String(), // references latest upstream.
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, upstream1.String(), nil)
	require.NoError(t, err)

	// Expected order (newest first):
	// 1. "Fix for v2.0" (synthetic, on top — latest upstream, with overlay)
	// 2. "upstream: v2.0" (replayed with new parent)
	// 3. "Fix for v1.0" (synthetic, interleaved after upstream v1.0)
	// 4. "upstream: v1.0" (import-commit, kept as-is)
	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	require.Len(t, logCommits, 4, "should have 2 upstream + 2 synthetic commits")

	assert.Contains(t, logCommits[0].Message, "Fix for v2.0")   // top synthetic (latest)
	assert.Contains(t, logCommits[1].Message, "upstream: v2.0") // replayed upstream 2
	assert.Contains(t, logCommits[2].Message, "Fix for v1.0")   // interleaved synthetic
	assert.Contains(t, logCommits[3].Message, "upstream: v1.0") // import-commit (kept)
}

func TestCommitInterleavedHistory_SingleCommit(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte("Name: package\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream, err := worktree.Commit("upstream: initial", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Modify working tree (simulates overlay application).
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\n# modified\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "abc123",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2025, 1, 10, 10, 0, 0, 0, time.UTC).Unix(),
				Message:     "Fix build",
			},
			UpstreamCommit: upstream.String(),
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, "", nil)
	require.NoError(t, err)

	// Verify working tree changes are in the single synthetic commit.
	head, err := repo.Head()
	require.NoError(t, err)

	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	assert.Contains(t, headCommit.Message, "Fix build")
	assert.Equal(t, "Alice", headCommit.Author.Name)

	// Verify file content was committed.
	tree, err := headCommit.Tree()
	require.NoError(t, err)

	entry, err := tree.File("package.spec")
	require.NoError(t, err)

	content, err := entry.Contents()
	require.NoError(t, err)
	assert.Contains(t, content, "# modified")
}

func TestCommitInterleavedHistory_OrphanUpstreamCommit(t *testing.T) {
	// When a fingerprint change references an upstream commit that doesn't
	// exist in the dist-git history, it should be dropped (not appended).
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte("Name: package\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream, err := worktree.Commit("upstream: initial", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-orphan",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Fix for unknown upstream",
			},
			UpstreamCommit: "deadbeefdeadbeef", // not in dist-git history.
		},
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-latest",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Latest fix",
			},
			UpstreamCommit: upstream.String(), // latest.
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, "", nil)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	// Only the latest-upstream synthetic commit is included; orphan is dropped.
	require.Len(t, logCommits, 2)
	assert.Contains(t, logCommits[0].Message, "Latest fix")
	assert.Equal(t, "upstream: initial", logCommits[1].Message)
}

func TestCommitInterleavedHistory_LocalComponent(t *testing.T) {
	// Local components have no upstream commits — all fingerprint changes
	// have empty UpstreamCommit. The initial commit acts as the root and
	// all synthetic commits are appended on top.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Create an initial commit (simulates initSourcesRepo).
	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte("Name: local-package\nVersion: 1.0\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	_, err = worktree.Commit("Initial sources", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "azldev",
			Email: "azldev@localhost",
			When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Simulate overlay modification in working tree.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: local-package\nVersion: 1.0\n# overlays applied\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	// All changes have empty UpstreamCommit (local component).
	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "local-aaa",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Add overlay config",
			},
			UpstreamCommit: "", // local — no upstream.
		},
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "local-bbb",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Bump release",
			},
			UpstreamCommit: "", // local — no upstream.
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, "", nil)
	require.NoError(t, err)

	// Verify: initial commit + 2 synthetic = 3 commits.
	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	require.Len(t, logCommits, 3, "should have initial + 2 synthetic commits")

	// Most recent (Bob's synthetic commit with overlay content).
	assert.Contains(t, logCommits[0].Message, "Bump release")
	assert.Equal(t, "Bob", logCommits[0].Author.Name)

	// Alice's synthetic commit (empty — only the last carries overlay tree).
	assert.Contains(t, logCommits[1].Message, "Add overlay config")
	assert.Equal(t, "Alice", logCommits[1].Author.Name)

	// Initial sources commit.
	assert.Equal(t, "Initial sources", logCommits[2].Message)

	// Verify the last synthetic commit has the overlay content.
	tree, err := logCommits[0].Tree()
	require.NoError(t, err)

	entry, err := tree.File("package.spec")
	require.NoError(t, err)

	content, err := entry.Contents()
	require.NoError(t, err)
	assert.Contains(t, content, "# overlays applied")
}

func TestCommitInterleavedHistory_MergeCommitInUpstream(t *testing.T) {
	// When the upstream dist-git contains merge commits, the replay should
	// linearize them: follow only first parents and preserve the merge
	// commit's tree content.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Upstream commit A (root).
	fileA, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = fileA.Write([]byte("Name: package\nVersion: 1.0\n"))
	require.NoError(t, err)
	require.NoError(t, fileA.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	commitA, err := worktree.Commit("upstream: v1.0", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	commitAObj, err := repo.CommitObject(commitA)
	require.NoError(t, err)

	// Upstream commit B (child of A, on main branch).
	fileB, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = fileB.Write([]byte("Name: package\nVersion: 2.0\n"))
	require.NoError(t, err)
	require.NoError(t, fileB.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	commitB, err := worktree.Commit("upstream: v2.0", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	commitBObj, err := repo.CommitObject(commitB)
	require.NoError(t, err)

	// Create a side-branch commit F (parent: A) to serve as second parent of merge.
	featureAuthor := object.Signature{
		Name:  "Feature",
		Email: "feature@fedora.org",
		When:  time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
	}

	featureCommitObj := &object.Commit{
		Author:       featureAuthor,
		Committer:    featureAuthor,
		Message:      "feature: add widget",
		TreeHash:     commitAObj.TreeHash,
		ParentHashes: []plumbing.Hash{commitA},
	}

	featureEncoded := repo.Storer.NewEncodedObject()
	err = featureCommitObj.Encode(featureEncoded)
	require.NoError(t, err)

	featureHash, err := repo.Storer.SetEncodedObject(featureEncoded)
	require.NoError(t, err)

	// Create merge commit M (parents: [B, F], tree: B's tree).
	mergeAuthor := object.Signature{
		Name:  "Upstream",
		Email: "upstream@fedora.org",
		When:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
	}

	mergeCommitObj := &object.Commit{
		Author:       mergeAuthor,
		Committer:    mergeAuthor,
		Message:      "Merge branch 'feature'",
		TreeHash:     commitBObj.TreeHash,
		ParentHashes: []plumbing.Hash{commitB, featureHash},
	}

	mergeEncoded := repo.Storer.NewEncodedObject()
	err = mergeCommitObj.Encode(mergeEncoded)
	require.NoError(t, err)

	mergeHash, err := repo.Storer.SetEncodedObject(mergeEncoded)
	require.NoError(t, err)

	// Update HEAD to point to the merge commit.
	head, err := repo.Storer.Reference(plumbing.HEAD)
	require.NoError(t, err)

	branchName := head.Target()
	err = repo.Storer.SetReference(plumbing.NewHashReference(branchName, mergeHash))
	require.NoError(t, err)

	// Simulate overlay modification in working tree.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\nVersion: 2.0\n# overlay applied\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	// Fingerprint change references the merge commit as upstream.
	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-merge",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Fix for merged version",
			},
			UpstreamCommit: mergeHash.String(),
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, commitA.String(), nil)
	require.NoError(t, err)

	// Expected order (newest first):
	// 1. "Fix for merged version" (synthetic, with overlay content)
	// 2. "Merge branch 'feature'" (replayed merge, linearized)
	// 3. "upstream: v2.0" (replayed)
	// 4. "upstream: v1.0" (import-commit, kept as-is)
	// The side-branch commit F should NOT appear.
	newHead, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: newHead.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	require.Len(t, logCommits, 4, "should have 3 upstream (A, B, M linearized) + 1 synthetic")

	assert.Contains(t, logCommits[0].Message, "Fix for merged version") // synthetic
	assert.Contains(t, logCommits[1].Message, "Merge branch 'feature'") // linearized merge
	assert.Contains(t, logCommits[2].Message, "upstream: v2.0")         // replayed
	assert.Contains(t, logCommits[3].Message, "upstream: v1.0")         // import-commit

	// All replayed commits should have exactly 1 parent (linearized).
	for i := range 3 {
		assert.Len(t, logCommits[i].ParentHashes, 1,
			"commit %d (%s) should have exactly 1 parent", i, logCommits[i].Message)
	}

	// Verify the synthetic commit carries overlay content.
	tree, err := logCommits[0].Tree()
	require.NoError(t, err)

	entry, err := tree.File("package.spec")
	require.NoError(t, err)

	content, err := entry.Contents()
	require.NoError(t, err)
	assert.Contains(t, content, "# overlay applied")
}

func TestParseCommitMetadata(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    sources.CommitMetadata
		wantErr bool
	}{
		{
			name:  "valid output",
			input: "abc123def456\x00Alice\x00alice@example.com\x001706100000\x00Fix CVE-2025-1234",
			want: sources.CommitMetadata{
				Hash:        "abc123def456",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   1706100000,
				Message:     "Fix CVE-2025-1234",
			},
		},
		{
			name:    "too few fields",
			input:   "abc123\x00Alice\x00alice@example.com",
			wantErr: true,
		},
		{
			name:    "invalid timestamp",
			input:   "abc123\x00Alice\x00alice@example.com\x00not-a-number\x00Fix bug",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sources.ParseCommitMetadata(test.input)
			if test.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestBuildDirtyChange(t *testing.T) {
	tests := []struct {
		name                  string
		currentFingerprint    string
		headLock              *lockfile.ComponentLock
		currentUpstreamCommit string
		wantNil               bool
		wantUpstream          string
	}{
		{
			name:                  "empty fingerprint disables detection",
			currentFingerprint:    "",
			headLock:              &lockfile.ComponentLock{InputFingerprint: "sha256:abc123", UpstreamCommit: "old"},
			currentUpstreamCommit: "new",
			wantNil:               true,
		},
		{
			name:                  "matching fingerprint returns nil",
			currentFingerprint:    "sha256:abc123",
			headLock:              &lockfile.ComponentLock{InputFingerprint: "sha256:abc123", UpstreamCommit: "old"},
			currentUpstreamCommit: "new",
			wantNil:               true,
		},
		{
			name:                  "nil headLock returns nil",
			currentFingerprint:    "sha256:abc123",
			headLock:              nil,
			currentUpstreamCommit: "new",
			wantNil:               true,
		},
		{
			name:                  "different fingerprint uses current upstream commit",
			currentFingerprint:    "sha256:new",
			headLock:              &lockfile.ComponentLock{InputFingerprint: "sha256:old", UpstreamCommit: "old-commit"},
			currentUpstreamCommit: "new-commit",
			wantUpstream:          "new-commit",
		},
		{
			name:                  "uses current upstream even when it matches head lock",
			currentFingerprint:    "sha256:new",
			headLock:              &lockfile.ComponentLock{InputFingerprint: "sha256:old", UpstreamCommit: "same-commit"},
			currentUpstreamCommit: "same-commit",
			wantUpstream:          "same-commit",
		},
		{
			name:                  "empty current upstream preserved for local components",
			currentFingerprint:    "sha256:new",
			headLock:              &lockfile.ComponentLock{InputFingerprint: "sha256:old", UpstreamCommit: ""},
			currentUpstreamCommit: "",
			wantUpstream:          "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := sources.BuildDirtyChange(test.currentFingerprint, test.headLock, test.currentUpstreamCommit)

			if test.wantNil {
				assert.Nil(t, result)

				return
			}

			require.NotNil(t, result)
			assert.Equal(t, "dirty", result.Hash)
			assert.Equal(t, "azldev", result.Author)
			assert.Equal(t, "azldev@local", result.AuthorEmail)
			assert.Equal(t, "Local changes (uncommitted)", result.Message)
			assert.Equal(t, test.wantUpstream, result.UpstreamCommit)
			assert.NotZero(t, result.Timestamp)
		})
	}
}

// TestCommitInterleavedHistory_SyntheticCommitsCarryOverlayTree asserts that
// every synthetic commit in the rebuilt dist-git history carries the overlay
// tree, while upstream-derived commits (the kept import-commit and any
// replayed upstreams) preserve their original tree byte-for-byte.
//
// Rationale: rpmautospec walks the dist-git history looking for the seed
// commit that introduced '%autochangelog'. If only HEAD carries the overlay,
// the seed is HEAD itself and no per-commit entries are materialized above
// the static '%changelog' sidecar. Putting the overlay tree on every
// synthetic commit lets rpmautospec see the autochangelog flip as far back
// as the first synthetic commit, so each synthetic commit becomes a
// changelog entry.
func TestCommitInterleavedHistory_SyntheticCommitsCarryOverlayTree(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Upstream commit 1 — a distinct tree.
	file1, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file1.Write([]byte("Name: package\nVersion: 1.0\nRelease: 1\n"))
	require.NoError(t, err)
	require.NoError(t, file1.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream1Hash, err := worktree.Commit("upstream: v1.0", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	upstream1, err := repo.CommitObject(upstream1Hash)
	require.NoError(t, err)

	// Upstream commit 2 — a different tree.
	file2, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file2.Write([]byte("Name: package\nVersion: 2.0\nRelease: 1\n"))
	require.NoError(t, err)
	require.NoError(t, file2.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream2Hash, err := worktree.Commit("upstream: v2.0", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	upstream2, err := repo.CommitObject(upstream2Hash)
	require.NoError(t, err)

	require.NotEqual(t, upstream1.TreeHash, upstream2.TreeHash,
		"test setup: upstream trees must differ to make the assertion meaningful")

	// Simulate overlay modification in the working tree. This must produce a
	// tree distinct from any upstream tree so the HEAD assertion is meaningful.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\nVersion: 2.0\nRelease: 1\n# overlay-applied\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	// One synthetic change for each upstream commit. The first interleaves
	// between upstream1 and upstream2; the second appends on top of upstream2.
	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-aaa",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Apply patch on v1.0",
			},
			UpstreamCommit: upstream1Hash.String(),
		},
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-bbb",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Apply patch on v2.0",
			},
			UpstreamCommit: upstream2Hash.String(),
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, upstream1Hash.String(), nil)
	require.NoError(t, err)

	// Walk the rebuilt history newest-first.
	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	require.Len(t, logCommits, 4, "expected 2 upstream + 2 synthetic commits")

	// Expected sequence (newest first):
	//   logCommits[0]: synthetic "v2.0" (HEAD)         — tree == overlay.
	//   logCommits[1]: replayed upstream v2.0          — tree == upstream2.TreeHash.
	//   logCommits[2]: synthetic intermediate on v1.0  — tree == overlay (NOT upstream1).
	//   logCommits[3]: kept upstream v1.0 (import)     — tree == upstream1.TreeHash.

	headCommit := logCommits[0]

	assert.NotEqual(t, upstream1.TreeHash, headCommit.TreeHash,
		"HEAD synthetic commit must carry the overlay tree, not an upstream tree")
	assert.NotEqual(t, upstream2.TreeHash, headCommit.TreeHash,
		"HEAD synthetic commit must carry the overlay tree, not an upstream tree")

	// Cross-check via message that we have the right commit in each slot.
	assert.Contains(t, headCommit.Message, "Apply patch on v2.0",
		"sanity: HEAD should be the topmost synthetic commit")
	assert.Contains(t, logCommits[1].Message, "upstream: v2.0",
		"sanity: position 1 should be replayed upstream v2.0")
	assert.Contains(t, logCommits[2].Message, "Apply patch on v1.0",
		"sanity: position 2 should be the interleaved synthetic on v1.0")
	assert.Contains(t, logCommits[3].Message, "upstream: v1.0",
		"sanity: position 3 should be the kept import-commit")

	// Core invariant: upstream-derived commits preserve their original tree;
	// every synthetic commit (including intermediates) carries the overlay
	// tree so rpmautospec sees the autochangelog flip in every synthetic
	// commit and materializes a changelog entry for each one.
	assert.Equal(t, upstream2.TreeHash, logCommits[1].TreeHash,
		"replayed upstream v2.0 must preserve its original tree byte-for-byte")
	assert.Equal(t, headCommit.TreeHash, logCommits[2].TreeHash,
		"intermediate synthetic commit must carry the same overlay tree as HEAD")
	assert.NotEqual(t, upstream1.TreeHash, logCommits[2].TreeHash,
		"intermediate synthetic commit must NOT inherit the upstream tree")
	assert.NotEqual(t, upstream2.TreeHash, logCommits[2].TreeHash,
		"intermediate synthetic commit must NOT inherit the upstream tree")
	assert.Equal(t, upstream1.TreeHash, logCommits[3].TreeHash,
		"kept import-commit must preserve its original tree byte-for-byte")
}

// TestCommitInterleavedHistory_ReplayedUpstreamHasAutochangelogBody asserts
// that when the upstream history spans multiple versions, the replayed
// upstream commits (intermediate, not the kept import-commit) have their
// spec's '%changelog' body flipped to '%autochangelog' in the rebuilt tree.
//
// This closes the version-bump gap: without the flip, the static body in a
// replayed upstream commit would break the autochangelog chain, causing
// rpmautospec to stop walking history at that commit and drop all synthetic
// entries below it. The kept import-commit retains its original static body
// and acts as the seed where rpmautospec terminates the walk.
func TestCommitInterleavedHistory_ReplayedUpstreamHasAutochangelogBody(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// A sibling file present in every upstream tree. The tree-rebuild path
	// in flipUpstreamSpecChangelogInTree must preserve non-spec entries
	// byte-for-byte; regressions there would silently drop sidecar files
	// (sources, patches) from replayed upstream commits.
	const sidecarName = "sources"

	const sidecarContent = "SHA512 (package-1.0.tar.xz) = deadbeef\n"

	sidecarFile, err := memFS.Create(sidecarName)
	require.NoError(t, err)

	_, err = sidecarFile.Write([]byte(sidecarContent))
	require.NoError(t, err)
	require.NoError(t, sidecarFile.Close())

	// Upstream commit 1 — spec with a static %changelog body. This becomes
	// the kept import-commit (seed; not flipped).
	const importSpec = "Name: package\n" +
		"Version: 1.0\n" +
		"Release: 1%{?dist}\n" +
		"Summary: Test\n" +
		"License: MIT\n" +
		"\n" +
		"%description\n" +
		"Test.\n" +
		"\n" +
		"%changelog\n" +
		"* Mon Jan 01 2024 Upstream <up@fedora.org> - 1.0-1\n" +
		"- import-commit static entry\n"

	writeSpecAndCommit(t, worktree, memFS, "package.spec", importSpec,
		"upstream: v1.0",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	head, err := repo.Head()
	require.NoError(t, err)

	upstream1Hash := head.Hash()
	upstream1, err := repo.CommitObject(upstream1Hash)
	require.NoError(t, err)

	// Upstream commit 2 — spec with a different static %changelog body.
	// This will be replayed; its body must be flipped to %autochangelog.
	const replayedSpec = "Name: package\n" +
		"Version: 2.0\n" +
		"Release: 1%{?dist}\n" +
		"Summary: Test\n" +
		"License: MIT\n" +
		"\n" +
		"%description\n" +
		"Test.\n" +
		"\n" +
		"%changelog\n" +
		"* Sat Jun 01 2024 Upstream <up@fedora.org> - 2.0-1\n" +
		"- replayed upstream static entry\n" +
		"\n" +
		"* Mon Jan 01 2024 Upstream <up@fedora.org> - 1.0-1\n" +
		"- import-commit static entry\n"

	writeSpecAndCommit(t, worktree, memFS, "package.spec", replayedSpec,
		"upstream: v2.0",
		time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))

	head, err = repo.Head()
	require.NoError(t, err)

	upstream2Hash := head.Hash()
	upstream2, err := repo.CommitObject(upstream2Hash)
	require.NoError(t, err)

	// Overlay state: working tree already has %autochangelog body (the
	// flip that tryMaterializeStaticChangelog applies before synthetic
	// history is built).
	const overlaySpec = "Name: package\n" +
		"Version: 2.0\n" +
		"Release: %autorelease\n" +
		"Summary: Test\n" +
		"License: MIT\n" +
		"\n" +
		"%description\n" +
		"Test.\n" +
		"\n" +
		"%changelog\n" +
		"%autochangelog\n"

	overlayFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = overlayFile.Write([]byte(overlaySpec))
	require.NoError(t, err)
	require.NoError(t, overlayFile.Close())

	// The static-changelog materialization step writes a 'changelog' sidecar
	// alongside the spec. The synthetic-history code is expected to inject
	// the same sidecar blob into every replayed upstream commit so that
	// rpmautospec's sidecar-boundary check doesn't terminate the walk above
	// the kept import-commit.
	const changelogSidecarContent = "* Mon Jan 01 2024 Pre <pre@example.com> - 0.9-1\n- ancient static entry\n"

	sidecar, err := memFS.Create("changelog")
	require.NoError(t, err)

	_, err = sidecar.Write([]byte(changelogSidecarContent))
	require.NoError(t, err)
	require.NoError(t, sidecar.Close())

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-aaa",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Apply patch on v1.0",
			},
			UpstreamCommit: upstream1Hash.String(),
		},
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "proj-bbb",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Apply patch on v2.0",
			},
			UpstreamCommit: upstream2Hash.String(),
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, upstream1Hash.String(), nil)
	require.NoError(t, err)

	// Walk newest-first.
	finalHead, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: finalHead.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	require.Len(t, logCommits, 4)

	// Position 1 is replayed upstream v2.0. Its tree must differ from the
	// original (the spec blob was replaced) and its spec %changelog body
	// must be '%autochangelog'.
	replayed := logCommits[1]
	require.Contains(t, replayed.Message, "upstream: v2.0",
		"sanity: position 1 should be replayed upstream v2.0")

	assert.NotEqual(t, upstream2.TreeHash, replayed.TreeHash,
		"replayed upstream tree must differ from the original (spec body flipped)")

	replayedSpecBody := readSpecFromTree(t, repo, replayed.TreeHash, "package.spec")
	assert.Contains(t, replayedSpecBody, "%autochangelog",
		"replayed upstream spec must have %%autochangelog in its %%changelog body")
	assert.NotContains(t, replayedSpecBody, "replayed upstream static entry",
		"replayed upstream spec must not retain the original static %%changelog body")

	// Non-spec entries (sidecar files like 'sources', patches, etc.) must
	// survive the tree rebuild byte-for-byte. Regressions in the
	// tree-mutation path would silently drop them.
	replayedSidecar := readSpecFromTree(t, repo, replayed.TreeHash, sidecarName)
	assert.Equal(t, sidecarContent, replayedSidecar,
		"replayed upstream tree must preserve non-spec sibling files byte-for-byte")

	// The 'changelog' sidecar (from the overlay tree) must also be injected
	// into the replayed upstream tree so rpmautospec's sidecar-boundary
	// check doesn't terminate the walk above the kept import-commit.
	replayedChangelogSidecar := readSpecFromTree(t, repo, replayed.TreeHash, "changelog")
	assert.Equal(t, changelogSidecarContent, replayedChangelogSidecar,
		"replayed upstream tree must carry the same 'changelog' sidecar blob as HEAD")

	// Position 3 is the kept import-commit. Its tree must be untouched.
	importCommit := logCommits[3]
	require.Contains(t, importCommit.Message, "upstream: v1.0",
		"sanity: position 3 should be the kept import-commit")

	assert.Equal(t, upstream1.TreeHash, importCommit.TreeHash,
		"kept import-commit tree must be unchanged (acts as the autochangelog-walk seed)")

	importSpecBody := readSpecFromTree(t, repo, importCommit.TreeHash, "package.spec")
	assert.Contains(t, importSpecBody, "import-commit static entry",
		"kept import-commit spec must retain its original static %%changelog body")
	assert.NotContains(t, importSpecBody, "%autochangelog",
		"kept import-commit spec must NOT contain %%autochangelog")

	// The kept import-commit must NOT carry the 'changelog' sidecar — its
	// absence is exactly what marks it as the seed boundary where
	// rpmautospec terminates the walk.
	importTree, err := repo.TreeObject(importCommit.TreeHash)
	require.NoError(t, err)

	for i := range importTree.Entries {
		assert.NotEqual(t, "changelog", importTree.Entries[i].Name,
			"kept import-commit tree must NOT contain the 'changelog' sidecar (acts as the seed boundary)")
	}
}

// writeSpecAndCommit overwrites the named spec in memFS, stages all working
// tree changes (so sibling files written into memFS also land in the commit),
// and creates a commit with the given message and author timestamp.
func writeSpecAndCommit(
	t *testing.T,
	worktree *gogit.Worktree,
	memFS billy.Filesystem,
	specName, content, message string,
	when time.Time,
) {
	t.Helper()

	f, err := memFS.Create(specName)
	require.NoError(t, err)

	_, err = f.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = worktree.Add(specName)
	require.NoError(t, err)

	// Pick up any sibling files (e.g., a 'sources' sidecar) that the test
	// wrote into memFS before calling this helper.
	err = worktree.AddWithOptions(&gogit.AddOptions{All: true})
	require.NoError(t, err)

	_, err = worktree.Commit(message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "up@fedora.org",
			When:  when,
		},
	})
	require.NoError(t, err)
}

// readSpecFromTree fetches the named spec blob from the given tree and
// returns its contents as a string.
func readSpecFromTree(t *testing.T, repo *gogit.Repository, treeHash plumbing.Hash, name string) string {
	t.Helper()

	tree, err := repo.TreeObject(treeHash)
	require.NoError(t, err)

	for i := range tree.Entries {
		e := &tree.Entries[i]
		if e.Name != name {
			continue
		}

		blob, err := repo.BlobObject(e.Hash)
		require.NoError(t, err)

		reader, err := blob.Reader()
		require.NoError(t, err)

		defer reader.Close()

		buf := make([]byte, blob.Size)
		_, err = reader.Read(buf)
		require.NoError(t, err)

		return string(buf)
	}

	t.Fatalf("spec %q not found in tree %s", name, treeHash)

	return ""
}

func TestCommitInterleavedHistory_BumpInjection(t *testing.T) {
	// Verify that the bumps map injects extra synth commits after the
	// anchor commit with [skip changelog] markers.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Create an upstream commit (import-commit / seed).
	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte("Name: package\nVersion: 1.0\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream1, err := worktree.Commit("upstream: initial", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Simulate overlay modification so the staging commit has content.
	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\nVersion: 1.0\n# overlays\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	upstreamHash := upstream1.String()

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "aaa111",
				Author:      "Alice",
				AuthorEmail: "alice@example.com",
				Timestamp:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Config change",
			},
			UpstreamCommit: upstreamHash,
		},
	}

	// Inject 3 bump commits anchored to the upstream commit.
	bumps := map[string]int{upstreamHash: 3}

	err = sources.CommitInterleavedHistory(repo, changes, "", bumps)
	require.NoError(t, err)

	// Walk the log: expect upstream(1) + bumps(3) + synthetic(1) = 5 commits.
	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var logCommits []*object.Commit

	err = commitIter.ForEach(func(c *object.Commit) error {
		logCommits = append(logCommits, c)

		return nil
	})
	require.NoError(t, err)

	assert.Len(t, logCommits, 5, "upstream(1) + bumps(3) + synthetic(1)")

	// logCommits is newest-first. Head = synthetic, then 3 bumps, then upstream.
	// Bump commits should contain [skip changelog] and the anchor short hash.
	shortAnchor := upstreamHash[:7]

	for _, c := range logCommits[1:4] {
		assert.Contains(t, c.Message, "[skip changelog]",
			"bump commit must contain [skip changelog] marker")
		assert.Contains(t, c.Message, shortAnchor,
			"bump commit must reference the anchor short hash")
		assert.Contains(t, c.Message, "bump",
			"bump commit must say 'bump'")
	}

	// Bump commits should carry the overlay tree (flipped spec), not the
	// upstream commit's original tree. The overlay tree is the same as the
	// final synthetic commit's tree (HEAD).
	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	for _, c := range logCommits[1:4] {
		assert.Equal(t, headCommit.TreeHash, c.TreeHash,
			"bump commits must carry the overlay tree (flipped spec)")
	}
}

func TestCommitInterleavedHistory_BumpUnmatchedAnchorWarns(t *testing.T) {
	// An anchor hash not present in the replayed history should be skipped
	// (no panic, no error) — just a warning. We verify no error is returned.
	memFS := memfs.New()
	storer := memory.NewStorage()

	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	file, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = file.Write([]byte("Name: package\nVersion: 1.0\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = worktree.Add("package.spec")
	require.NoError(t, err)

	upstream1, err := worktree.Commit("upstream: initial", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Upstream",
			Email: "upstream@fedora.org",
			When:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	specFile, err := memFS.Create("package.spec")
	require.NoError(t, err)

	_, err = specFile.Write([]byte("Name: package\nVersion: 1.0\n# overlay\n"))
	require.NoError(t, err)
	require.NoError(t, specFile.Close())

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "bbb222",
				Author:      "Bob",
				AuthorEmail: "bob@example.com",
				Timestamp:   time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "Change",
			},
			UpstreamCommit: upstream1.String(),
		},
	}

	// Anchor hash that doesn't exist in the replayed history.
	bumps := map[string]int{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef": 10}

	err = sources.CommitInterleavedHistory(repo, changes, "", bumps)
	require.NoError(t, err, "unmatched anchors should warn, not error")

	// Should still have the normal commit count (no bump commits injected).
	head, err := repo.Head()
	require.NoError(t, err)

	commitIter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)

	count := 0

	err = commitIter.ForEach(func(c *object.Commit) error {
		count++

		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, 2, count, "upstream(1) + synthetic(1), no bump commits")
}
