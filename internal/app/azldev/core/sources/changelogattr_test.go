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

// TestParseChangelogEntries_StandardFormat verifies the parser handles a
// typical multi-entry rpm %changelog body: newest-first, with version
// suffixes and body bullets.
func TestParseChangelogEntries_StandardFormat(t *testing.T) {
	body := []string{
		"* Tue Jan 07 2025 Bob Real <bob@example.com> - 1.0-2",
		"- bug fix for issue 42",
		"- also tightened the loop",
		"",
		"* Mon Jan 06 2025 Alice Real <alice@example.com> - 1.0-1",
		"- initial release",
	}

	entries, unparsed := sources.ParseChangelogEntries(body)
	require.Empty(t, unparsed, "no headers should fail to parse")
	require.Len(t, entries, 2)

	// Newest first (rpm convention).
	assert.Equal(t, "Bob Real", entries[0].Author)
	assert.Equal(t, "bob@example.com", entries[0].Email)
	assert.Equal(t, "1.0-2", entries[0].Version)
	assert.Equal(t,
		time.Date(2025, 1, 7, 0, 0, 0, 0, time.UTC),
		entries[0].Date)
	assert.Equal(t,
		[]string{"- bug fix for issue 42", "- also tightened the loop", ""},
		entries[0].Body)

	assert.Equal(t, "Alice Real", entries[1].Author)
	assert.Equal(t, "alice@example.com", entries[1].Email)
	assert.Equal(t, "1.0-1", entries[1].Version)
	assert.Equal(t,
		time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC),
		entries[1].Date)
	assert.Equal(t, []string{"- initial release"}, entries[1].Body)
}

// TestParseChangelogEntries_NoVersion verifies entries without a trailing
// version suffix are parsed correctly.
func TestParseChangelogEntries_NoVersion(t *testing.T) {
	body := []string{
		"* Mon Jan 06 2025 Alice <alice@example.com>",
		"- something",
	}

	entries, unparsed := sources.ParseChangelogEntries(body)
	require.Empty(t, unparsed)
	require.Len(t, entries, 1)
	assert.Equal(t, "Alice", entries[0].Author)
	assert.Equal(t, "alice@example.com", entries[0].Email)
	assert.Empty(t, entries[0].Version)
}

// TestNewChangelogEntries_DiffByNormalizedKey verifies the diff returns
// entries that are new in current vs parent, matched by normalized key
// (epoch + email + body), in chronological order (oldest first).
func TestNewChangelogEntries_DiffByNormalizedKey(t *testing.T) {
	parent := []sources.ChangelogEntry{
		{
			Date:  time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC),
			Email: "alice@example.com",
			Body:  []string{"- initial"},
		},
	}

	current := []sources.ChangelogEntry{
		// Newest first (rpm convention).
		{
			Date:  time.Date(2025, 1, 9, 0, 0, 0, 0, time.UTC),
			Email: "dan@example.com",
			Body:  []string{"- third fix"},
		},
		{
			Date:  time.Date(2025, 1, 7, 0, 0, 0, 0, time.UTC),
			Email: "bob@example.com",
			Body:  []string{"- second fix"},
		},
		{
			Date:  time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC),
			Email: "alice@example.com",
			Body:  []string{"- initial"},
		},
	}

	diff := sources.NewChangelogEntries(parent, current)
	require.Len(t, diff, 2)
	// Oldest first in the diff (so callers replay chronologically).
	assert.Equal(t, "bob@example.com", diff[0].Email)
	assert.Equal(t, "dan@example.com", diff[1].Email)
}

// TestNewChangelogEntries_NormalizedKey_IgnoresDatePadding proves the
// date-padding dedup fix: `Fri Feb 06 2026` and `Fri Feb 6 2026` are the
// same epoch and so the same entry.
func TestNewChangelogEntries_NormalizedKey_IgnoresDatePadding(t *testing.T) {
	sharedDate := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	parent := []sources.ChangelogEntry{
		{
			Header: "* Fri Feb 6 2026 Dalibor <d@x> - 1.33",
			Date:   sharedDate,
			Email:  "d@x",
			Body:   []string{"- foo"},
		},
	}

	current := []sources.ChangelogEntry{
		{
			Header: "* Fri Feb 06 2026 Dalibor <d@x> - 1.33-2", // zero-padded day
			Date:   sharedDate,
			Email:  "d@x",
			Body:   []string{"- foo"},
		},
	}

	diff := sources.NewChangelogEntries(parent, current)
	assert.Empty(t, diff, "date-padding-only differences should dedup")
}

// TestAttributeFromTrees_PrefersChangelogAuthor exercises the end-to-end
// goal: an upstream commit authored by a bot ("Robot <bot@fedora>") that
// adds a single %changelog entry attributed to a real human should yield a
// CommitMetadata with the human's name/email/date — not the bot's.
//
// This is the primary use case driving the feature: fedora dist-git commits
// frequently have bot or maintainer-of-record git authorship while the real
// patch authorship lives in the %changelog entry body and header.
func TestAttributeFromTrees_PrefersChangelogAuthor(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()
	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Parent commit: spec with one changelog entry.
	parentSpec := minSpec("1%{?dist}",
		"* Mon Jan 06 2025 Alice Real <alice@example.com> - 1.0-1",
		"- initial release",
	)
	writeFile(t, memFS, "pkg.spec", parentSpec)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)

	botSig := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC),
	}
	parentHash, err := worktree.Commit("import: 1.0-1", &gogit.CommitOptions{Author: botSig, Committer: botSig})
	require.NoError(t, err)

	// Child commit: same bot author, but adds a new %changelog entry from
	// a real human.
	childSpec := minSpec("2%{?dist}",
		"* Tue Jan 07 2025 Bob Real <bob@example.com> - 1.0-2",
		"- bug fix for issue 42",
		"",
		"* Mon Jan 06 2025 Alice Real <alice@example.com> - 1.0-1",
		"- initial release",
	)
	writeFile(t, memFS, "pkg.spec", childSpec)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)

	childBotSig := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 7, 12, 0, 0, 0, time.UTC),
	}
	childHash, err := worktree.Commit("import: 1.0-2", &gogit.CommitOptions{Author: childBotSig, Committer: childBotSig})
	require.NoError(t, err)

	parentCommit, err := repo.CommitObject(parentHash)
	require.NoError(t, err)

	childCommit, err := repo.CommitObject(childHash)
	require.NoError(t, err)

	metas := sources.AttributeFromTrees(repo, childCommit.TreeHash, parentCommit.TreeHash)
	require.Len(t, metas, 1, "expected one synth commit per new changelog entry")

	got := metas[0]
	assert.Equal(t, "Bob Real", got.Author, "author should come from changelog entry, not bot")
	assert.Equal(t, "bob@example.com", got.AuthorEmail, "email should come from changelog entry, not bot")
	assert.Equal(t,
		time.Date(2025, 1, 7, 12, 0, 0, 0, time.UTC).Unix(),
		got.Timestamp,
		"timestamp should come from changelog entry date (librpm-canonical noon UTC), not bot commit time")
	assert.Contains(t, got.Message, "bug fix for issue 42",
		"message should contain the changelog entry body, not the bot commit message")
}

// TestAttributeFromTrees_FallbackWhenNoChangelogChange exercises the
// fallback path: an upstream commit that doesn't touch the %changelog
// section should yield nil (caller falls back to upstream git metadata).
func TestAttributeFromTrees_FallbackWhenNoChangelogChange(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()
	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	parentSpec := minSpec("1%{?dist}",
		"* Mon Jan 06 2025 Alice <alice@example.com> - 1.0-1",
		"- initial",
	)
	writeFile(t, memFS, "pkg.spec", parentSpec)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)

	sig := &object.Signature{Name: "Robot", Email: "bot@fedora.org", When: time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC)}
	parentHash, err := worktree.Commit("init", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	// Child: touches a non-spec file. %changelog unchanged.
	writeFile(t, memFS, "patches/fix.patch", "diff data")

	_, err = worktree.Add("patches/fix.patch")
	require.NoError(t, err)

	childHash, err := worktree.Commit("add patch", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	parentCommit, err := repo.CommitObject(parentHash)
	require.NoError(t, err)

	childCommit, err := repo.CommitObject(childHash)
	require.NoError(t, err)

	metas := sources.AttributeFromTrees(repo, childCommit.TreeHash, parentCommit.TreeHash)
	assert.Nil(t, metas, "no %changelog change should yield nil (callers fall back)")
}

// joinLines joins lines with '\n' and appends a trailing newline so the
// resulting blob looks like a real text file.
func joinLines(lines ...string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}

	return out
}

// minSpec wraps changelog entry lines in a minimal spec preamble that
// rpmspec will accept (Summary/License/%description are mandatory).
func minSpec(release string, changelogLines ...string) string {
	lines := []string{
		"Name: pkg",
		"Version: 1.0",
		"Release: " + release,
		"Summary: t",
		"License: MIT",
		"",
		"%description",
		"T.",
		"",
		"%changelog",
	}
	lines = append(lines, changelogLines...)

	return joinLines(lines...)
}

// writeFile writes content to a file in the billy fs.
func writeFile(t *testing.T, fs billy.Filesystem, path, content string) {
	t.Helper()

	f, err := fs.Create(path)
	require.NoError(t, err)

	_, err = f.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// TestCommitInterleavedHistory_FallbackPreservesUpstreamMetadata is an
// end-to-end check on the fallback path through the production call chain
// (CommitInterleavedHistory → replayUpstreamWithContract → fallback).
//
// Scenario: two upstream commits where the second commit modifies a patch
// file but does NOT touch the spec's %changelog. AttributeFromTrees
// returns nil for both replays (root for the seed; no diff for commit 2),
// so the fallback branch fires twice. The test asserts that the replayed
// upstream commits preserve the original upstream author, committer, and
// commit message verbatim — distinct author vs committer, distinct
// message text — proving the fallback path is byte-faithful and not
// silently coerced through the attribution metadata (which loses
// committer-vs-author distinction).
func TestCommitInterleavedHistory_FallbackPreservesUpstreamMetadata(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()
	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Distinct author and committer on every upstream commit. Both must
	// survive the fallback path verbatim.
	botAuthor := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC),
	}
	maintainer := &object.Signature{
		Name:  "Maintainer",
		Email: "maintainer@example.com",
		When:  time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC),
	}

	// Commit 1 (seed): spec WITH a %changelog section. Seed's parent is
	// ZeroHash so attribution returns nil → fallback fires.
	specV1 := minSpec("1%{?dist}",
		"* Mon Jan 06 2025 Alice <alice@example.com> - 1.0-1",
		"- initial",
	)
	writeFile(t, memFS, "pkg.spec", specV1)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)

	seedMessage := "import: initial release"

	seedHash, err := worktree.Commit(seedMessage, &gogit.CommitOptions{
		Author:    botAuthor,
		Committer: maintainer,
	})
	require.NoError(t, err)

	// Commit 2: add a patch file, do NOT touch the spec. The %changelog
	// is byte-identical between commit 1 and commit 2 → AttributeFromTrees
	// returns nil → fallback fires.
	writeFile(t, memFS, "fix.patch", "diff data\n")

	_, err = worktree.Add("fix.patch")
	require.NoError(t, err)

	patchBotAuthor := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 7, 9, 30, 0, 0, time.UTC),
	}
	patchMaintainer := &object.Signature{
		Name:  "Maintainer",
		Email: "maintainer@example.com",
		When:  time.Date(2025, 1, 7, 10, 45, 0, 0, time.UTC),
	}
	patchMessage := "import: backport upstream fix\n\nDetails about the fix go here."

	patchHash, err := worktree.Commit(patchMessage, &gogit.CommitOptions{
		Author:    patchBotAuthor,
		Committer: patchMaintainer,
	})
	require.NoError(t, err)

	// One synthetic fingerprint change pinned at commit 2.
	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "synth1",
				Author:      "azldev",
				AuthorEmail: "azldev@local",
				Timestamp:   time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "azldev overlay",
			},
			UpstreamCommit: patchHash.String(),
		},
	}

	// Disable Contract flips: we want raw replays so the fallback's
	// fidelity isn't entangled with the Contract.Materialize rewrites.
	err = sources.CommitInterleavedHistory(repo, changes, "", nil, false, false, false)
	require.NoError(t, err)

	// Walk the resulting history: synth (top) → commit 2 replay →
	// commit 1 replay → nil.
	head, err := repo.Head()
	require.NoError(t, err)

	synth, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	require.Len(t, synth.ParentHashes, 1, "synth commit should have one parent")

	replayedCommit2, err := repo.CommitObject(synth.ParentHashes[0])
	require.NoError(t, err)

	// Assert commit 2's fallback: upstream metadata preserved verbatim.
	assert.Equal(t, patchBotAuthor.Name, replayedCommit2.Author.Name,
		"commit 2 author name should be preserved by fallback")
	assert.Equal(t, patchBotAuthor.Email, replayedCommit2.Author.Email,
		"commit 2 author email should be preserved by fallback")
	assert.True(t, patchBotAuthor.When.Equal(replayedCommit2.Author.When),
		"commit 2 author time should be preserved by fallback (not coerced to date-only)")
	assert.Equal(t, patchMaintainer.Name, replayedCommit2.Committer.Name,
		"commit 2 committer name should be preserved by fallback (distinct from author)")
	assert.Equal(t, patchMaintainer.Email, replayedCommit2.Committer.Email,
		"commit 2 committer email should be preserved by fallback")
	assert.True(t, patchMaintainer.When.Equal(replayedCommit2.Committer.When),
		"commit 2 committer time should be preserved by fallback")
	assert.Equal(t, patchMessage, replayedCommit2.Message,
		"commit 2 message should be preserved verbatim by fallback")

	require.Len(t, replayedCommit2.ParentHashes, 1, "commit 2 replay should have one parent (the seed)")

	replayedSeed, err := repo.CommitObject(replayedCommit2.ParentHashes[0])
	require.NoError(t, err)

	// Assert seed's fallback: upstream metadata preserved verbatim.
	// Seed exercises the root-commit ZeroHash-parent branch in AttributeFromTrees.
	assert.Equal(t, botAuthor.Name, replayedSeed.Author.Name,
		"seed author name should be preserved by fallback")
	assert.Equal(t, botAuthor.Email, replayedSeed.Author.Email,
		"seed author email should be preserved by fallback")
	assert.True(t, botAuthor.When.Equal(replayedSeed.Author.When),
		"seed author time should be preserved by fallback")
	assert.Equal(t, maintainer.Name, replayedSeed.Committer.Name,
		"seed committer name should be preserved by fallback (distinct from author)")
	assert.Equal(t, maintainer.Email, replayedSeed.Committer.Email,
		"seed committer email should be preserved by fallback")
	assert.Equal(t, seedMessage, replayedSeed.Message,
		"seed message should be preserved verbatim by fallback")

	// Tree fidelity: replayed commits should have the same tree hashes as
	// their upstream originals (no Contract flips configured in this test).
	upstreamSeed, err := repo.CommitObject(seedHash)
	require.NoError(t, err)
	upstreamCommit2, err := repo.CommitObject(patchHash)
	require.NoError(t, err)
	assert.Equal(t, upstreamSeed.TreeHash, replayedSeed.TreeHash,
		"seed tree should be preserved by fallback")
	assert.Equal(t, upstreamCommit2.TreeHash, replayedCommit2.TreeHash,
		"commit 2 tree should be preserved by fallback")
}

// TestCommitInterleavedHistory_MultipleEntriesFanOutIntoCommits exercises
// the multi-entry-added-in-one-upstream-commit case end-to-end. Three new
// %changelog entries appear in commit 2 vs commit 1; the replay should
// fan out into three synth commits, oldest first, each carrying the
// matching entry's author/email/date.
func TestCommitInterleavedHistory_MultipleEntriesFanOutIntoCommits(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()
	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	bot := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC),
	}

	// Commit 1: spec with one %changelog entry.
	specV1 := minSpec("1%{?dist}",
		"* Mon Jan 06 2025 Alice <alice@example.com> - 1.0-1",
		"- initial",
	)
	writeFile(t, memFS, "pkg.spec", specV1)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)
	_, err = worktree.Commit("import 1.0-1", &gogit.CommitOptions{Author: bot, Committer: bot})
	require.NoError(t, err)

	// Commit 2: three new entries (newest first per rpm convention),
	// each by a different human.
	specV2 := minSpec("4%{?dist}",
		"* Thu Jan 09 2025 Dan <dan@example.com> - 1.0-4",
		"- third fix",
		"",
		"* Wed Jan 08 2025 Carol <carol@example.com> - 1.0-3",
		"- second fix",
		"",
		"* Tue Jan 07 2025 Bob <bob@example.com> - 1.0-2",
		"- first fix",
		"",
		"* Mon Jan 06 2025 Alice <alice@example.com> - 1.0-1",
		"- initial",
	)
	writeFile(t, memFS, "pkg.spec", specV2)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)

	commit2Bot := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 9, 12, 0, 0, 0, time.UTC),
	}
	commit2Hash, err := worktree.Commit("import 1.0-4", &gogit.CommitOptions{Author: commit2Bot, Committer: commit2Bot})
	require.NoError(t, err)

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "synth1",
				Author:      "azldev",
				AuthorEmail: "azldev@local",
				Timestamp:   time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "overlay",
			},
			UpstreamCommit: commit2Hash.String(),
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, "", nil, false, false, false)
	require.NoError(t, err)

	// Walk: synth (top) ← Dan ← Carol ← Bob ← seed.
	// Replay of commit 2 fans into 3 attributed commits (oldest-first
	// during replay, so HEAD-walk reads them newest-first: Dan, Carol, Bob).
	head, err := repo.Head()
	require.NoError(t, err)

	synth, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	require.Len(t, synth.ParentHashes, 1)

	expected := []struct {
		name, email string
		date        time.Time
		bodyHint    string
	}{
		{"Dan", "dan@example.com", time.Date(2025, 1, 9, 12, 0, 0, 0, time.UTC), "third fix"},
		{"Carol", "carol@example.com", time.Date(2025, 1, 8, 12, 0, 0, 0, time.UTC), "second fix"},
		{"Bob", "bob@example.com", time.Date(2025, 1, 7, 12, 0, 0, 0, time.UTC), "first fix"},
	}

	cur := synth.ParentHashes[0]

	for idx, want := range expected {
		commitObj, err := repo.CommitObject(cur)
		require.NoError(t, err, "loading attributed commit %d", idx)
		assert.Equal(t, want.name, commitObj.Author.Name, "commit %d author name", idx)
		assert.Equal(t, want.email, commitObj.Author.Email, "commit %d author email", idx)
		assert.True(t, want.date.Equal(commitObj.Author.When),
			"commit %d author date (want %v, got %v)", idx, want.date, commitObj.Author.When)
		assert.Contains(t, commitObj.Message, want.bodyHint, "commit %d message", idx)
		// All fanned-out commits share the upstream commit 2's tree.
		upstreamC2, err := repo.CommitObject(commit2Hash)
		require.NoError(t, err)
		assert.Equal(t, upstreamC2.TreeHash, commitObj.TreeHash, "commit %d tree should match upstream", idx)
		require.Len(t, commitObj.ParentHashes, 1)
		cur = commitObj.ParentHashes[0]
	}

	// The chain after the 3 attributed commits should be the seed replay
	// (commit 1). Seed uses fallback (ZeroHash parent → no attribution).
	seed, err := repo.CommitObject(cur)
	require.NoError(t, err)
	assert.Equal(t, "Robot", seed.Author.Name, "seed should use upstream bot author via fallback")
}

// TestCommitInterleavedHistory_MalformedEntryFallsBack proves the
// "any fail → fallback" policy: if even one new %changelog entry header
// fails to parse, the entire upstream commit falls back to a single
// replay with the original upstream metadata, rather than producing
// partial attribution.
func TestCommitInterleavedHistory_MalformedEntryFallsBack(t *testing.T) {
	memFS := memfs.New()
	storer := memory.NewStorage()
	repo, err := gogit.Init(storer, memFS)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	bot := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC),
	}

	specV1 := minSpec("1%{?dist}",
		"* Mon Jan 06 2025 Alice <alice@example.com> - 1.0-1",
		"- initial",
	)
	writeFile(t, memFS, "pkg.spec", specV1)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)
	_, err = worktree.Commit("import 1.0-1", &gogit.CommitOptions{Author: bot, Committer: bot})
	require.NoError(t, err)

	// Commit 2: two new entries — one well-formed, one with an unparseable
	// month token ("Foo" is 3 word chars so it passes the regex, but
	// time.Parse rejects it as a month name).
	specV2 := minSpec("3%{?dist}",
		"* Wed Jan 08 2025 Carol <carol@example.com> - 1.0-3",
		"- good entry",
		"",
		"* Tue Foo 07 2025 Bob <bob@example.com> - 1.0-2",
		"- malformed month",
		"",
		"* Mon Jan 06 2025 Alice <alice@example.com> - 1.0-1",
		"- initial",
	)
	writeFile(t, memFS, "pkg.spec", specV2)

	_, err = worktree.Add("pkg.spec")
	require.NoError(t, err)

	commit2Bot := &object.Signature{
		Name:  "Robot",
		Email: "bot@fedora.org",
		When:  time.Date(2025, 1, 8, 12, 0, 0, 0, time.UTC),
	}
	commit2Message := "import multiple fixes (mixed quality changelog)"
	commit2Hash, err := worktree.Commit(commit2Message, &gogit.CommitOptions{Author: commit2Bot, Committer: commit2Bot})
	require.NoError(t, err)

	changes := []sources.FingerprintChange{
		{
			CommitMetadata: sources.CommitMetadata{
				Hash:        "synth1",
				Author:      "azldev",
				AuthorEmail: "azldev@local",
				Timestamp:   time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
				Message:     "overlay",
			},
			UpstreamCommit: commit2Hash.String(),
		},
	}

	err = sources.CommitInterleavedHistory(repo, changes, "", nil, false, false, false)
	require.NoError(t, err)

	// Walk: synth (top) ← single replayed commit 2 (fallback) ← seed.
	// If the policy had allowed partial attribution there would be 2
	// commits between synth and seed (Carol's good entry + something for
	// Bob's malformed entry). With "any fail → fallback", there is
	// exactly one replayed upstream commit and it carries the bot's
	// original metadata + message verbatim.
	head, err := repo.Head()
	require.NoError(t, err)
	synth, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	require.Len(t, synth.ParentHashes, 1)

	replayed, err := repo.CommitObject(synth.ParentHashes[0])
	require.NoError(t, err)

	assert.Equal(t, "Robot", replayed.Author.Name,
		"malformed entry should force fallback to upstream bot author")
	assert.Equal(t, "bot@fedora.org", replayed.Author.Email,
		"malformed entry should force fallback to upstream bot email")
	assert.Equal(t, commit2Message, replayed.Message,
		"malformed entry should force fallback to upstream commit message verbatim")

	// The next parent should be the seed — confirming there's exactly one
	// commit between the synth and the seed (no partial attribution).
	require.Len(t, replayed.ParentHashes, 1)

	seed, err := repo.CommitObject(replayed.ParentHashes[0])
	require.NoError(t, err)
	assert.Equal(t, "Robot", seed.Author.Name, "seed should also use fallback")
	// Seed's parent should be nil (root) — confirming exactly 3 commits in chain.
	assert.Empty(t, seed.ParentHashes, "seed should be the root of the synth history")
}

// Ensure plumbing import is used.
var _ = plumbing.ZeroHash
