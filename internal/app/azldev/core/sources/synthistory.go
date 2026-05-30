// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/git"
)

// CommitMetadata holds full metadata for a commit in the project repository.
type CommitMetadata struct {
	Hash        string
	Author      string
	AuthorEmail string
	Timestamp   int64
	Message     string
}

// FingerprintChange records a project commit that changed a component's lock file
// fingerprint. [UpstreamCommit] is the value of the 'upstream-commit' field in the
// lock file at the point of the change.
type FingerprintChange struct {
	CommitMetadata

	// UpstreamCommit is the upstream dist-git commit hash recorded in the lock
	// file at the time the fingerprint changed.
	UpstreamCommit string
}

// interleavedEntry represents a single commit in the rebuilt dist-git history.
// Exactly one of upstreamCommit or syntheticChange is non-nil.
type interleavedEntry struct {
	upstreamCommit  *object.Commit
	syntheticChange *FingerprintChange
}

// FindFingerprintChanges walks the git log of the project repository for commits
// that changed the given lock file and returns metadata for each commit where the
// 'input-fingerprint' field changed. Results are sorted chronologically (oldest
// first).
func FindFingerprintChanges(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	projectRepo *gogit.Repository,
	projectRepoDir string,
	lockFileRelPath string,
) ([]FingerprintChange, error) {
	// Get commit metadata (newest-first) for all commits that touched the lock file.
	metas, err := gitLogFileMetadata(ctx, cmdFactory, projectRepoDir, lockFileRelPath)
	if err != nil {
		return nil, err
	}

	if len(metas) == 0 {
		return nil, nil
	}

	// Pair each commit's metadata with its lock file contents.
	type entry struct {
		lock lockfile.ComponentLock
		meta CommitMetadata
	}

	var entries []entry //nolint:prealloc // size not known ahead of time.

	for _, meta := range metas {
		lock, err := lockfile.ShowAtCommit(projectRepo, meta.Hash, lockFileRelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read lock file at commit %#q:\n%w", meta.Hash, err)
		}

		entries = append(entries, entry{lock: lock, meta: meta})
	}

	if len(entries) == 0 {
		return nil, nil
	}

	// Entries are newest-first (from git log order). Reverse to chronological.
	slices.Reverse(entries)

	// Walk chronologically and detect fingerprint changes.
	var changes []FingerprintChange

	prevFingerprint := ""

	for _, change := range entries {
		if change.lock.InputFingerprint != prevFingerprint {
			changes = append(changes, FingerprintChange{
				CommitMetadata: change.meta,
				UpstreamCommit: change.lock.UpstreamCommit,
			})
		}

		prevFingerprint = change.lock.InputFingerprint
	}

	return changes, nil
}

// CommitInterleavedHistory rebuilds the dist-git history by interleaving
// synthetic commits with the existing upstream commits. Synthetic commits
// referencing an older upstream commit are placed directly after that commit;
// those referencing the latest upstream commit are appended on top. The very
// last synthetic commit carries the overlay file changes; all others are empty.
//
// When importCommit is non-empty, only upstream commits from importCommit
// onward are considered for interleaving.
//
// The bumps map, when non-nil, injects additional synth "bump" commits at
// anchor points during replay. Each key is an upstream commit hash; its value
// is the number of bump commits to inject right after that upstream commit.
// Bump commits satisfy the rpmautospec contract via [Contract.Materialize]
// and carry the [SkipChangelogMarker] via [Contract.CommitMessage].
//
// replaceRelease and replaceChangelog control whether the spec's Release tag and
// %changelog body are rewritten to %autorelease / %autochangelog during
// replay. Both should be false for components configured with manual release
// AND manual changelog calculation — injecting auto* macros into a manual
// spec triggers rpmautospec to walk the entire upstream history.
//
// truncateUpstreamHistory, when true, makes the replayed seed a root commit
// (cuts the upstream parent chain). See [replaySeedWithContract] for
// when this opt-in workaround is needed.
func CommitInterleavedHistory(
	repo *gogit.Repository,
	changes []FingerprintChange,
	importCommit string,
	bumps map[string]int,
	replaceRelease, replaceChangelog, truncateUpstreamHistory bool,
) error {
	// No changes means no synthetic commits to create, so skip the whole process.
	if len(changes) == 0 {
		return nil
	}

	// The latest fingerprint change's UpstreamCommit is the commit we're
	// pinned to — use it as the upper bound for the upstream walk instead
	// of HEAD, which may be ahead (e.g., at the branch tip).
	upstreamCommit := changes[len(changes)-1].UpstreamCommit

	// Collect upstream boundary commits BEFORE staging, so the temporary
	// commit created by stageAndCaptureOverlayTree is not included.
	//
	// Collapsed mode: instead of walking every intermediate upstream commit
	// between import-commit and upstream-commit, collect only the two
	// boundary commits (import + tip). Intermediate commits add no value
	// for static-changelog packages — the sidecar provides pre-import
	// history, attribution diffs the import spec vs upstream spec for
	// post-import entries, and bumps handle release numbering.
	//
	// This reduces a 6000-commit kernel history to at most 2 commits.
	upstreamCommits, skippedCount, err := collectBoundaryCommits(repo, importCommit, upstreamCommit)
	if err != nil {
		return err
	}

	if skippedCount > 0 {
		slog.Info("Collapsed upstream history",
			"importCommit", safeShortHash(importCommit, shortHashLen),
			"upstreamCommit", safeShortHash(upstreamCommit, shortHashLen),
			"skippedCommits", skippedCount)
	}

	// Stage overlay changes and capture the resulting tree hash.
	overlayTreeHash, err := stageAndCaptureOverlayTree(repo)
	if err != nil {
		return err
	}

	// Build the full interleaved sequence of upstream and synthetic commits.
	sequence := buildInterleavedSequence(upstreamCommits, changes)

	return replayInterleavedHistory(
		repo, sequence, overlayTreeHash, bumps,
		replaceRelease, replaceChangelog, truncateUpstreamHistory,
	)
}

// stageAndCaptureOverlayTree stages all working tree changes and creates a
// temporary commit to capture the resulting tree hash. The tree hash is used
// later to set the content of the final synthetic commit.
func stageAndCaptureOverlayTree(repo *gogit.Repository) (plumbing.Hash, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get worktree:\n%w", err)
	}

	if err := worktree.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to stage changes:\n%w", err)
	}

	tempHash, err := worktree.Commit("temp: capture overlay tree", &gogit.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "azldev", When: time.Unix(0, 0).UTC()},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to create temporary commit:\n%w", err)
	}

	tempCommit, err := repo.CommitObject(tempHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to read temporary commit:\n%w", err)
	}

	return tempCommit.TreeHash, nil
}

// buildInterleavedSequence produces the full commit sequence for the rebuilt
// history. Upstream commits appear in chronological order; synthetic commits
// that reference an older upstream are inserted directly after it. Synthetic
// commits referencing the latest upstream are appended at the end.
// Changes with no upstream commit (local components) are always placed on top.
// Orphaned commits whose non-empty upstream is not found in the dist-git
// history are dropped with a warning.
func buildInterleavedSequence(
	upstreamCommits []*object.Commit,
	changes []FingerprintChange,
) []interleavedEntry {
	latestUpstream := changes[len(changes)-1].UpstreamCommit

	var interleaved, top []FingerprintChange

	for idx := range changes {
		switch changes[idx].UpstreamCommit {
		case "":
			// Local component changes have no upstream commit reference.
			// Always place them on top of the history.
			top = append(top, changes[idx])
		case latestUpstream:
			top = append(top, changes[idx])
		default:
			interleaved = append(interleaved, changes[idx])
		}
	}

	// Build a lookup from upstream-commit hash → synthetic commits.
	interleavedByUpstream := make(map[string][]FingerprintChange)

	for i := range interleaved {
		hash := interleaved[i].UpstreamCommit
		interleavedByUpstream[hash] = append(interleavedByUpstream[hash], interleaved[i])
	}

	// Walk upstream commits, inserting synthetics after their referenced commit.
	sequence := make([]interleavedEntry, 0, len(upstreamCommits)+len(changes))

	for i := range upstreamCommits {
		sequence = append(sequence, interleavedEntry{upstreamCommit: upstreamCommits[i]})

		hash := upstreamCommits[i].Hash.String()
		if synthetics, ok := interleavedByUpstream[hash]; ok {
			for j := range synthetics {
				synth := synthetics[j]
				sequence = append(sequence, interleavedEntry{syntheticChange: &synth})
			}

			delete(interleavedByUpstream, hash)
		}
	}

	// Remaining interleaved changes reference upstream commits not found in
	// the dist-git history — drop them with a warning. Will be useful for when we switch branches.
	for hash, orphaned := range interleavedByUpstream {
		slog.Warn("Upstream commit referenced by fingerprint change not found in dist-git history; "+
			"dropping",
			"upstreamCommit", hash,
			"count", len(orphaned))
	}

	// Append "top" synthetic commits at the end.
	for i := range top {
		topChange := top[i]
		sequence = append(sequence, interleavedEntry{syntheticChange: &topChange})
	}

	return sequence
}

// replayInterleavedHistory walks the interleaved sequence and creates new
// commit objects with correct tree hashes and parent chains. The seed
// (import-commit) is replayed via [replaySeedWithContract]; post-seed
// upstream commits are replayed via [replayUpstreamWithContract]. All trees
// are materialized via [Contract] to satisfy the rpmautospec invariants.
// Bump commits are injected after their anchor upstream commit.
func replayInterleavedHistory(
	repo *gogit.Repository,
	sequence []interleavedEntry,
	overlayTreeHash plumbing.Hash,
	bumps map[string]int,
	replaceRelease, replaceChangelog, truncateUpstreamHistory bool,
) error {
	syntheticCount := countSyntheticEntries(sequence)

	sidecarBlobHash, err := LookupOverlaySidecarBlob(repo, overlayTreeHash)
	if err != nil {
		return err
	}

	contract := Contract{
		ReplaceRelease:   replaceRelease,
		ReplaceChangelog: replaceChangelog,
		SidecarBlob:      sidecarBlobHash,
	}

	// Track which bump anchors have been consumed.
	consumedAnchors := make(map[string]bool, len(bumps))

	var (
		lastHash     plumbing.Hash
		syntheticIdx int
		startIdx     int
	)

	// Replay the seed (import-commit). Its upstream parents are preserved
	// unless truncateUpstreamHistory is set (see [replaySeedWithContract]).
	if len(sequence) > 0 && sequence[0].upstreamCommit != nil {
		seedCommit := sequence[0].upstreamCommit

		replayedSeedHash, replayErr := replaySeedWithContract(repo, seedCommit, contract, truncateUpstreamHistory)
		if replayErr != nil {
			return replayErr
		}

		lastHash = replayedSeedHash
		startIdx = 1

		var bumpErr error

		lastHash, bumpErr = tryInjectBumps(repo, lastHash, seedCommit.Hash.String(),
			bumps, consumedAnchors, contract, overlayTreeHash)
		if bumpErr != nil {
			return bumpErr
		}
	}

	for idx := startIdx; idx < len(sequence); idx++ {
		var replayErr error

		lastHash, syntheticIdx, replayErr = replayEntry(
			repo, sequence[idx], lastHash, overlayTreeHash,
			bumps, consumedAnchors, contract,
			syntheticIdx, syntheticCount)
		if replayErr != nil {
			return replayErr
		}
	}

	warnUnmatchedBumps(bumps, consumedAnchors)

	if err := updateHead(repo, lastHash); err != nil {
		return err
	}

	// Reset the index and working tree to match the new HEAD. The temporary
	// commit created by stageAndCaptureOverlayTree leaves the index pointing
	// at the pre-Materialize overlay tree. Without a hard reset, rpmautospec
	// sees staged diffs (the Contract rewrites) and emits a spurious
	// "Uncommitted changes" changelog entry.
	if err := resetWorktreeToHead(repo); err != nil {
		return err
	}

	slog.Info("Interleaved synthetic history complete",
		"syntheticCommits", syntheticCount,
		"totalCommits", len(sequence))

	return nil
}

// replayEntry replays a single interleaved entry (upstream or synthetic).
// Returns the updated lastHash, syntheticIdx, and any error.
func replayEntry(
	repo *gogit.Repository,
	entry interleavedEntry,
	lastHash, overlayTreeHash plumbing.Hash,
	bumps map[string]int,
	consumedAnchors map[string]bool,
	contract Contract,
	syntheticIdx, syntheticCount int,
) (plumbing.Hash, int, error) {
	if entry.upstreamCommit != nil {
		replayedHash, replayErr := replayUpstreamWithContract(repo, entry.upstreamCommit, lastHash, contract)
		if replayErr != nil {
			return plumbing.ZeroHash, syntheticIdx, replayErr
		}

		lastHash = replayedHash

		var bumpErr error

		lastHash, bumpErr = tryInjectBumps(repo, lastHash, entry.upstreamCommit.Hash.String(),
			bumps, consumedAnchors, contract, overlayTreeHash)
		if bumpErr != nil {
			return plumbing.ZeroHash, syntheticIdx, bumpErr
		}

		return lastHash, syntheticIdx, nil
	}

	syntheticIdx++

	// Choose the input tree for this synth commit:
	//
	//   - Last synth: use the current overlay tree. This is the only commit
	//     whose tree should reflect the live state of the working dir
	//     (latest version bump, overlay edits, etc).
	//
	//   - Non-last synth: inherit the parent commit's tree (upstream or
	//     prior synth). This preserves the version-in-effect at each
	//     project commit's point in history — the rendered changelog
	//     shows "this AZL change was made while tracking upstream X.Y.Z"
	//     rather than retroactively claiming the current overlay version
	//     for all historical entries.
	//
	// Both paths go through Contract.Materialize so macro flips
	// (Release→%autorelease, %changelog→%autochangelog, sidecar) happen
	// uniformly on every commit's spec.
	isLast := syntheticIdx == syntheticCount
	inputTree := overlayTreeHash

	if !isLast {
		parentCommit, parentErr := repo.CommitObject(lastHash)
		if parentErr != nil {
			return plumbing.ZeroHash, syntheticIdx,
				fmt.Errorf("read parent commit %s for synthetic tree inheritance:\n%w", lastHash, parentErr)
		}

		inputTree = parentCommit.TreeHash
	}

	synthTree, materializeErr := contract.Materialize(repo, inputTree)
	if materializeErr != nil {
		return plumbing.ZeroHash, syntheticIdx, fmt.Errorf("materialize contract for synthetic commit:\n%w", materializeErr)
	}

	hash, synthErr := createSyntheticCommit(repo, entry.syntheticChange, synthTree, lastHash,
		syntheticIdx, syntheticCount)
	if synthErr != nil {
		return plumbing.ZeroHash, syntheticIdx, synthErr
	}

	return hash, syntheticIdx, nil
}

// warnUnmatchedBumps logs a warning for any bump anchors not consumed during replay.
func warnUnmatchedBumps(bumps map[string]int, consumedAnchors map[string]bool) {
	for anchor, count := range bumps {
		if count > 0 && !consumedAnchors[anchor] {
			slog.Warn("Bump anchor not found in replayed history; skipping",
				"anchor", anchor,
				"count", count)
		}
	}
}

// replaySeedWithContract recreates the seed (import-commit) with a
// contract-satisfying tree.
//
// When truncateUpstreamHistory is false (the default), the seed preserves
// ALL of its upstream parents so rpmautospec can walk the original
// upstream history when computing %autorelease's release_number. This
// matches the behavior of older azldev versions and is the right default
// for new packages — they get a natural release number derived from
// upstream commit count.
//
// When true (opt-in via [projectconfig.ReleaseConfig.TruncateUpstreamHistory]),
// the seed becomes a ROOT commit — rpmautospec walks only our synth chain.
// This is the workaround for packages where rpmautospec hangs walking the
// full upstream history (e.g. kernel's %define %rpmversion trips the spec
// parser on every walked commit in rpmautospec 0.8.3). The trade-off is
// that %autorelease's release_number drops to the count of synth commits
// only; use lock-file bumps to compensate.
func replaySeedWithContract(
	repo *gogit.Repository,
	commit *object.Commit,
	contract Contract,
	truncateUpstreamHistory bool,
) (plumbing.Hash, error) {
	newTree, err := contract.Materialize(repo, commit.TreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("materialize contract for seed commit:\n%w", err)
	}

	var parentHashes []plumbing.Hash
	if !truncateUpstreamHistory {
		parentHashes = commit.ParentHashes
	}

	slog.Debug("Replaying seed commit",
		"seed", commit.Hash,
		"truncated", truncateUpstreamHistory,
		"parentCount", len(parentHashes))

	hash, err := createCommitObject(repo, newTree,
		commit.Author, commit.Committer, commit.Message,
		parentHashes...)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to replay seed commit:\n%w", err)
	}

	return hash, nil
}

// replayUpstreamWithContract recreates an upstream commit with a contract-
// satisfying tree and a new parent. Merge commits are linearized. The
// upstream commit's git author/committer/message are preserved verbatim.
func replayUpstreamWithContract(
	repo *gogit.Repository,
	commit *object.Commit,
	parentHash plumbing.Hash,
	contract Contract,
) (plumbing.Hash, error) {
	newTree, err := contract.Materialize(repo, commit.TreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("materialize contract for replayed upstream:\n%w", err)
	}

	if len(commit.ParentHashes) > 1 {
		slog.Debug("Linearizing merge commit in upstream history",
			"commit", commit.Hash,
			"parentCount", len(commit.ParentHashes))
	}

	hash, err := createCommitObject(repo, newTree,
		commit.Author, commit.Committer, commit.Message,
		parentHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to replay upstream commit:\n%w", err)
	}

	return hash, nil
}

// tryInjectBumps checks if the bumps map has an entry for commitHash. If so,
// it injects the corresponding bump commits and marks the anchor as consumed.
//
// overlayTreeHash is the final overlay-applied tree (HEAD's tree). Bumps use
// this as their base instead of the upstream parent tree so the rendered
// spec filename (e.g. after a spec-set-tag Name rename overlay) is present
// in the bump's tree. Without this, rpmautospec walks looking for the
// renamed spec at HEAD and skips bumps that only have the original
// upstream filename — leaving the release counter short.
func tryInjectBumps(
	repo *gogit.Repository,
	lastHash plumbing.Hash,
	commitHash string,
	bumps map[string]int,
	consumedAnchors map[string]bool,
	contract Contract,
	overlayTreeHash plumbing.Hash,
) (plumbing.Hash, error) {
	count, ok := bumps[commitHash]
	if !ok || count <= 0 {
		return lastHash, nil
	}

	bumpHash, err := replayBumpCommits(repo, lastHash, commitHash, count, contract, overlayTreeHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	consumedAnchors[commitHash] = true

	return bumpHash, nil
}

// replayBumpCommits injects count synth "bump" commits right after the anchor.
// Each commit's tree is produced by [Contract.Materialize] on overlayTreeHash
// so the bump tree carries the final rendered spec (including any rename
// overlays). The commit message carries the [SkipChangelogMarker] via
// [Contract.CommitMessage].
func replayBumpCommits(
	repo *gogit.Repository,
	parentHash plumbing.Hash,
	anchorHash string,
	count int,
	contract Contract,
	overlayTreeHash plumbing.Hash,
) (plumbing.Hash, error) {
	// Materialize a contract-satisfying tree from the overlay tree. Using
	// the overlay tree (not the upstream parent's tree) ensures bumps have
	// the renamed spec filename so rpmautospec counts them toward the
	// release_number when walking from HEAD.
	bumpContract := contract
	bumpContract.SkipChangelog = true

	bumpTree, err := bumpContract.Materialize(repo, overlayTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("materialize contract for bump commits:\n%w", err)
	}

	shortAnchor := safeShortHash(anchorHash, shortHashLen)

	current := parentHash

	for idx := 1; idx <= count; idx++ {
		msg := bumpContract.CommitMessage(fmt.Sprintf("bump %s (%d/%d)", shortAnchor, idx, count))

		author := object.Signature{
			Name:  "azldev",
			Email: "azldev@local",
			When:  time.Unix(0, 0).UTC(),
		}

		hash, createErr := createCommitObject(repo, bumpTree, author, author, msg, current)
		if createErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("creating bump commit %d/%d for anchor %s:\n%w",
				idx, count, shortAnchor, createErr)
		}

		slog.Debug("Created bump commit",
			"anchor", shortAnchor,
			"bump", fmt.Sprintf("%d/%d", idx, count))

		current = hash
	}

	return current, nil
}

// createSyntheticCommit creates a synthetic commit from a [FingerprintChange],
// logging progress information.
func createSyntheticCommit(
	repo *gogit.Repository,
	change *FingerprintChange,
	treeHash, parentHash plumbing.Hash,
	syntheticIdx, syntheticCount int,
) (plumbing.Hash, error) {
	author := object.Signature{
		Name:  change.Author,
		Email: change.AuthorEmail,
		When:  unixToTime(change.Timestamp),
	}

	message := fmt.Sprintf("%s\n\nProject commit: %s", change.Message, change.Hash)

	slog.Info("Creating synthetic commit",
		"commit", syntheticIdx,
		"total", syntheticCount,
		"projectHash", change.Hash,
		"upstreamCommit", change.UpstreamCommit,
		"isLast", syntheticIdx == syntheticCount,
	)

	hash, err := createCommitObject(repo, treeHash, author, author, message, parentHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to create synthetic commit %d:\n%w", syntheticIdx, err)
	}

	return hash, nil
}

// countSyntheticEntries returns the number of synthetic entries in the sequence.
func countSyntheticEntries(sequence []interleavedEntry) int {
	count := 0

	for _, entry := range sequence {
		if entry.syntheticChange != nil {
			count++
		}
	}

	return count
}

// createCommitObject creates a new commit in the repository's object store with
// the given tree, parents, author, committer, and message. Variadic parents
// support merge commits with multiple parents; pass [plumbing.ZeroHash] (or
// nothing) for root commits. Zero-hash entries are filtered out.
func createCommitObject(
	repo *gogit.Repository,
	treeHash plumbing.Hash,
	author, committer object.Signature,
	message string,
	parentHashes ...plumbing.Hash,
) (plumbing.Hash, error) {
	commit := &object.Commit{
		Author:    author,
		Committer: committer,
		Message:   message,
		TreeHash:  treeHash,
	}

	// Filter out zero-hash parents so go-git's log iterator doesn't choke on
	// root commits that pass them as placeholders.
	for _, p := range parentHashes {
		if p != plumbing.ZeroHash {
			commit.ParentHashes = append(commit.ParentHashes, p)
		}
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit:\n%w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit:\n%w", err)
	}

	return hash, nil
}

// updateHead updates the HEAD reference (or the branch it points to) to the
// given commit hash.
func updateHead(repo *gogit.Repository, commitHash plumbing.Hash) error {
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return fmt.Errorf("failed to read HEAD reference:\n%w", err)
	}

	// Resolve symbolic ref (e.g., HEAD → refs/heads/main).
	name := plumbing.HEAD
	if head.Type() != plumbing.HashReference {
		name = head.Target()
	}

	ref := plumbing.NewHashReference(name, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update HEAD to %s:\n%w", commitHash, err)
	}

	return nil
}

// resetWorktreeToHead performs a hard reset so the index and working tree match
// the current HEAD commit. This is necessary after [updateHead] repoints HEAD
// to a replayed commit whose tree was rewritten by [Contract.Materialize].
func resetWorktreeToHead(repo *gogit.Repository) error {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree for reset:\n%w", err)
	}

	if err := wt.Reset(&gogit.ResetOptions{Mode: gogit.HardReset}); err != nil {
		return fmt.Errorf("failed to hard-reset worktree to HEAD:\n%w", err)
	}

	return nil
}

// buildSyntheticCommits resolves the project repository from the component's
// config file, walks the lock file's git history for fingerprint changes, and
// returns the matching [FingerprintChange] entries sorted chronologically.
// Returns (nil, "", nil) when there are no changes to represent — either the
// lock file is missing, has no fingerprint changes (with a warning), or the
// current fingerprint matches the committed one.
//
// When currentFingerprint is non-empty it is compared against the fingerprint
// stored in the HEAD commit's lock file. If they differ a "dirty" entry is
// appended so that uncommitted config/overlay changes are represented in the
// synthetic history.
//
// The lockDir is the absolute path to the lock file directory. It is converted
// to a repo-relative path internally once the git repository root is known.
func buildSyntheticCommits(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	config *projectconfig.ComponentConfig,
	componentName string,
	lockDir string,
	currentFingerprint string,
) (changes []FingerprintChange, importCommit string, err error) {
	projectRepo, projectRepoDir, err := openProjectRepo(config, componentName)
	if err != nil {
		return nil, "", err
	}

	if projectRepo == nil {
		return nil, "", nil
	}

	// Compute the lock file path relative to the git repository root.
	lockFileAbsPath, err := lockfile.LockPath(lockDir, componentName)
	if err != nil {
		return nil, "", fmt.Errorf("resolving lock file path for %#q:\n%w", componentName, err)
	}

	lockFileRelPath, err := filepath.Rel(projectRepoDir, lockFileAbsPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to compute repo-relative lock path for %#q:\n%w",
			lockFileAbsPath, err)
	}

	// Read the lock file at HEAD. If the file is missing (not yet committed),
	// synthetic history is skipped.
	headLock, err := readLockFileAtHEAD(projectRepo, lockFileRelPath)
	if err != nil {
		return nil, "", err
	}

	if headLock == nil {
		return nil, "", nil
	}

	importCommit = headLock.ImportCommit

	fpChanges, err := FindFingerprintChanges(ctx, cmdFactory, projectRepo, projectRepoDir, lockFileRelPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to find fingerprint changes for lock file %#q:\n%w",
			lockFileRelPath, err)
	}

	// In a shallow clone the commit that added the lock file may have been
	// pruned. Detect this before falling through to dirty detection.
	if len(fpChanges) == 0 {
		shallowCommits, _ := projectRepo.Storer.Shallow()
		if len(shallowCommits) > 0 {
			return nil, "", fmt.Errorf(
				"lock file %#q has no git history; a full clone is required",
				lockFileRelPath)
		}
	}

	// Check for uncommitted ("dirty") changes by comparing the caller-provided
	// current fingerprint against the fingerprint stored in the HEAD lock file.
	if dirty := BuildDirtyChange(currentFingerprint, headLock, config.EffectiveUpstreamCommit()); dirty != nil {
		slog.Info("Current fingerprint differs from HEAD lock file; adding dirty entry",
			"lockFile", lockFileRelPath)

		fpChanges = append(fpChanges, *dirty)
	}

	if len(fpChanges) == 0 {
		slog.Warn("Lock file has no fingerprint changes; skipping synthetic history",
			"lockFile", lockFileRelPath)

		return nil, "", nil
	}

	return fpChanges, importCommit, nil
}

// BuildDirtyChange returns a [FingerprintChange] representing uncommitted
// config/overlay changes. Returns nil when currentFingerprint is empty or
// matches the HEAD lock file's fingerprint.
//
// currentUpstreamCommit is the effective upstream commit from the on-disk
// (possibly uncommitted) lock file. This is used instead of
// [headLock.UpstreamCommit] so that upstream commit changes from
// 'component update' (written to disk but not yet committed) are reflected
// in the dirty entry.
func BuildDirtyChange(
	currentFingerprint string,
	headLock *lockfile.ComponentLock,
	currentUpstreamCommit string,
) *FingerprintChange {
	if currentFingerprint == "" {
		return nil
	}

	if headLock == nil || headLock.InputFingerprint == "" {
		return nil
	}

	if currentFingerprint == headLock.InputFingerprint {
		return nil
	}

	slog.Debug("Dirty fingerprint detected",
		"current", currentFingerprint,
		"head", headLock.InputFingerprint)

	return &FingerprintChange{
		CommitMetadata: CommitMetadata{
			Hash:        "dirty",
			Author:      "azldev",
			AuthorEmail: "azldev@local",
			Timestamp:   time.Now().Unix(),
			Message:     "Local changes (uncommitted)",
		},
		UpstreamCommit: currentUpstreamCommit,
	}
}

// openProjectRepo opens the git repository that contains the component's
// config file and returns both the [gogit.Repository] and the worktree root
// directory. Returns (nil, "", nil) when the config file path cannot be
// resolved, indicating that synthetic commits should be skipped.
func openProjectRepo(
	config *projectconfig.ComponentConfig,
	componentName string,
) (*gogit.Repository, string, error) {
	if config.SourceConfigFile == nil || config.SourceConfigFile.SourcePath() == "" {
		slog.Debug("Cannot resolve config file for synthetic commits; skipping",
			"component", componentName)

		return nil, "", nil
	}

	configFilePath := config.SourceConfigFile.SourcePath()

	repo, err := git.OpenProjectRepo(filepath.Dir(configFilePath))
	if err != nil {
		return nil, "", fmt.Errorf("failed to find project repository for config file %#q:\n%w",
			configFilePath, err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get project worktree:\n%w", err)
	}

	return repo, worktree.Filesystem.Root(), nil
}

// readLockFileAtHEAD reads the lock file at the repository's HEAD commit.
// Returns nil (without error) when the lock file or its parent directory does
// not exist in the commit tree — this is the normal case for components that
// have never had overlays. Returns a non-nil error for real failures (TOML
// parse errors, unexpected git object errors, etc.).
func readLockFileAtHEAD(
	repo *gogit.Repository,
	lockFileRelPath string,
) (*lockfile.ComponentLock, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD:\n%w", err)
	}

	headLock, lockFileErr := lockfile.ShowAtCommit(repo, head.Hash().String(), lockFileRelPath)
	if lockFileErr == nil {
		return &headLock, nil
	}

	// Tolerate both file-not-found and directory-not-found — the latter
	// occurs when the locks directory has never been created in the repo.
	if !errors.Is(lockFileErr, object.ErrFileNotFound) &&
		!errors.Is(lockFileErr, object.ErrDirectoryNotFound) {
		return nil, fmt.Errorf("failed to read lock file %#q at HEAD:\n%w",
			lockFileRelPath, lockFileErr)
	}

	// File genuinely missing — no committed lock at HEAD. This is normal
	// for local components (no upstream commit) and for upstream components
	// whose lock was created on disk but not yet committed to git.
	// Note: the resolver validates lock existence on the *filesystem* (working
	// tree), not in git — so a lock written by 'component update' but not
	// yet committed passes resolver validation but is absent at HEAD.
	slog.Debug("No lock file found at HEAD; skipping synthetic history",
		"lockFile", lockFileRelPath, "reason", lockFileErr)

	return nil, nil //nolint:nilnil // nil,nil signals "not found, skip" to caller.
}

// collectBoundaryCommits returns at most two upstream commits: the
// import-commit (seed) and, when different, the upstream-commit (tip).
// Intermediate commits are skipped — their count is returned so callers
// can log the savings. This is the "collapsed" upstream history mode.
//
// When importCommit == upstreamCommit (or upstreamCommit is empty), only
// the import-commit is returned with skippedCount=0.
func collectBoundaryCommits(
	repo *gogit.Repository, importCommit, upstreamCommit string,
) ([]*object.Commit, int, error) {
	if importCommit == "" {
		// No import boundary — fall back to the full walk.
		commits, err := collectUpstreamCommits(repo, importCommit, upstreamCommit)

		return commits, 0, err
	}

	importObj, err := repo.CommitObject(plumbing.NewHash(importCommit))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read import-commit %#q:\n%w", importCommit, err)
	}

	// Same commit or no separate upstream → seed only.
	if upstreamCommit == "" || upstreamCommit == importCommit {
		return []*object.Commit{importObj}, 0, nil
	}

	upstreamObj, err := repo.CommitObject(plumbing.NewHash(upstreamCommit))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read upstream-commit %#q:\n%w", upstreamCommit, err)
	}

	// Count how many commits we're skipping (for logging).
	skipped, err := countCommitsBetween(repo, importCommit, upstreamCommit)
	if err != nil {
		// Non-fatal: we can still proceed without the count.
		slog.Debug("Could not count skipped commits", "err", err)

		skipped = -1
	}

	// Chronological order: seed first, then tip.
	return []*object.Commit{importObj, upstreamObj}, skipped, nil
}

// countCommitsBetween counts the number of first-parent commits strictly
// between two commit hashes (exclusive of both endpoints). Returns -1 on
// error (non-fatal — callers use this for logging only).
func countCommitsBetween(repo *gogit.Repository, olderHash, newerHash string) (int, error) {
	count := 0
	currentHash := plumbing.NewHash(newerHash)

	for {
		commit, err := repo.CommitObject(currentHash)
		if err != nil {
			return -1, fmt.Errorf("load commit %s:\n%w", currentHash, err)
		}

		if len(commit.ParentHashes) == 0 {
			break
		}

		parentHash := commit.ParentHashes[0]
		if parentHash.String() == olderHash {
			return count, nil
		}

		count++
		currentHash = parentHash
	}

	return count, fmt.Errorf("older commit %s not reachable from %s", olderHash, newerHash)
}

// collectUpstreamCommits returns commits in the repository in chronological
// order (oldest first), bounded by importCommit (inclusive start) and
// upstreamCommit (inclusive end). Only first-parent links are followed so that
// merge commits are included but side-branch commits are excluded, producing a
// linear mainline history suitable for replay.
//
// This is the full-walk fallback used by [collectBoundaryCommits] when
// importCommit is empty. Normal operation uses the collapsed two-commit path.
func collectUpstreamCommits(
	repo *gogit.Repository, importCommit, upstreamCommit string,
) ([]*object.Commit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD reference:\n%w", err)
	}

	// Walk newest-first following only first parents.  Collect commits
	// between upstreamCommit (newest boundary) and importCommit (oldest).
	var (
		commits       []*object.Commit
		foundUpstream bool
		foundImport   bool
		collecting    = upstreamCommit == "" // if no upper bound, collect from start.
		currentHash   = head.Hash()
	)

	for {
		commit, err := repo.CommitObject(currentHash)
		if err != nil {
			return nil, fmt.Errorf("failed to read commit %#q:\n%w", currentHash.String(), err)
		}

		hash := commit.Hash.String()

		// Start collecting once we see the upstream-commit (newest boundary).
		if !collecting && hash == upstreamCommit {
			collecting = true
		}

		if collecting {
			commits = append(commits, commit)
		}

		if hash == upstreamCommit {
			foundUpstream = true
		}

		// Stop once we reach the import-commit (oldest boundary).
		if importCommit != "" && hash == importCommit {
			foundImport = true

			break
		}

		// Follow only the first parent to stay on the mainline.
		if len(commit.ParentHashes) == 0 {
			break
		}

		currentHash = commit.ParentHashes[0]
	}

	if upstreamCommit != "" && !foundUpstream {
		return nil, fmt.Errorf(
			"upstream-commit %#q not found in dist-git history; "+
				"the lock file may reference a commit from a different branch",
			upstreamCommit)
	}

	if importCommit != "" && !foundImport {
		return nil, fmt.Errorf(
			"import-commit %#q not found in dist-git history; "+
				"the repository may be a shallow clone or the commit may have been rebased away",
			importCommit)
	}

	// Walk was newest-first; reverse to chronological.
	slices.Reverse(commits)

	return commits, nil
}

// unixToTime converts a Unix timestamp to a [time.Time] in UTC.
func unixToTime(unix int64) time.Time {
	return time.Unix(unix, 0).UTC()
}

// --- git CLI helpers ---

// gitLogFileMetadata returns commit metadata (newest-first) for all commits
// that touched the given file path in the repository at repoDir. Fields within
// each record are separated by NUL (\x00); records are separated by SOH (\x01).
//
// This shells out to 'git log' rather than using go-git's [gogit.LogOptions]
// PathFilter because go-git's path filtering walks the entire commit graph
// in-process, diffing trees at every commit. For large repositories with
// thousands of commits this is prohibitively slow. The git CLI delegates the
// work to native C code with bitmap indices and pack-file optimizations,
// making it orders of magnitude faster for path-scoped log queries.
func gitLogFileMetadata(
	ctx context.Context, cmdFactory opctx.CmdFactory, repoDir, filePath string,
) ([]CommitMetadata, error) {
	output, err := git.RunInDir(ctx, cmdFactory, repoDir,
		"log", "--format=%H%x00%an%x00%ae%x00%at%x00%s%x01", "--", filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to list commits for %#q:\n%w", filePath, err)
	}

	if output == "" {
		return nil, nil
	}

	blocks := strings.Split(output, "\x01")

	var metas []CommitMetadata //nolint:prealloc // trailing empty block after split.

	for _, block := range blocks {
		block = strings.Trim(block, "\r\n")
		if block == "" {
			continue
		}

		meta, err := ParseCommitMetadata(block)
		if err != nil {
			return nil, fmt.Errorf("failed to parse commit metadata:\n%w", err)
		}

		metas = append(metas, meta)
	}

	return metas, nil
}

// commitMetadataFieldCount is the number of NUL-separated fields expected in
// a single commit record produced by 'git log --format=%H%x00%an%x00%ae%x00%at%x00%s'.
const commitMetadataFieldCount = 5

// ParseCommitMetadata parses a single NUL-delimited commit record produced by
// 'git log --format=%H%x00%an%x00%ae%x00%at%x00%s'.
func ParseCommitMetadata(output string) (CommitMetadata, error) {
	fields := strings.SplitN(output, "\x00", commitMetadataFieldCount)

	if len(fields) < commitMetadataFieldCount {
		return CommitMetadata{}, fmt.Errorf(
			"unexpected git log output (expected %d fields, got %d):\n%v",
			commitMetadataFieldCount, len(fields), output)
	}

	var timestamp int64
	if _, err := fmt.Sscanf(fields[3], "%d", &timestamp); err != nil {
		return CommitMetadata{}, fmt.Errorf("failed to parse timestamp %#q:\n%w", fields[3], err)
	}

	return CommitMetadata{
		Hash:        fields[0],
		Author:      fields[1],
		AuthorEmail: fields[2],
		Timestamp:   timestamp,
		Message:     fields[4],
	}, nil
}
