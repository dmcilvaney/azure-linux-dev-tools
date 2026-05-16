// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
)

// ChangelogSidecarFilename is the name of the file placed alongside the spec
// in the synthetic dist-git to carry pre-import changelog entries. rpmautospec
// reads this file during process-distgit and appends its contents below the
// entries it materializes from git history.
const ChangelogSidecarFilename = "changelog"

// Contract describes the rpmautospec invariants that every non-seed commit in
// a synthetic dist-git must satisfy. The four invariants are:
//
//  1. Spec '%changelog' body = '%autochangelog'
//  2. Spec 'Release' tag = '%autorelease'
//  3. 'changelog' sidecar file present (when SidecarBlob is set)
//  4. Bump commits carry [skip changelog] on its own line (via CommitMessage)
//
// The seed commit (import-commit, idx==0) is kept with its original tree.
// rpmautospec walks back TO this commit and stops. Everything above it must
// satisfy the contract.
//
// Use [Contract.Materialize] to produce a contract-satisfying tree from any
// input tree, and [Contract.CommitMessage] to decorate commit messages with
// the [skip changelog] marker for bump commits.
type Contract struct {
	// SidecarBlob is the changelog sidecar content hash. When non-zero, the
	// sidecar file is injected/updated in the tree. When zero, no sidecar
	// is touched (used for non-static changelog modes).
	SidecarBlob plumbing.Hash

	// SkipChangelog marks bump commits. When true, CommitMessage appends
	// the [skip changelog] marker to the commit message body.
	SkipChangelog bool
}

// Materialize returns a tree hash that satisfies the rpmautospec contract.
// It flips the top-level spec's '%changelog' body to '%autochangelog' and
// 'Release' to '%autorelease', and injects/updates the changelog sidecar
// when SidecarBlob is set. Each flip is attempted independently — a missing
// '%changelog' section does not prevent the Release flip, and vice versa.
//
// Idempotent: returns the input tree hash unchanged when the tree already
// satisfies the contract (byte-equal check on the serialized spec + sidecar
// presence/hash comparison).
//
// Non-spec entries in the tree are preserved byte-for-byte.
func (c Contract) Materialize(
	repo *gogit.Repository, treeHash plumbing.Hash,
) (plumbing.Hash, error) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read tree %s:\n%w", treeHash, err)
	}

	specEntry := findTopLevelSpec(tree)
	if specEntry == nil {
		return treeHash, nil
	}

	newSpecBlobHash, specChanged, err := flipSpecBlob(repo, specEntry)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	sidecarChanged := needsSidecarUpdate(tree, c.SidecarBlob)

	if !specChanged && !sidecarChanged {
		return treeHash, nil
	}

	newEntries := buildRewrittenEntries(tree.Entries, specEntry.Name,
		newSpecBlobHash, specChanged, c.SidecarBlob, sidecarChanged)

	newTree := &object.Tree{Entries: newEntries}
	newTreeObj := repo.Storer.NewEncodedObject()

	if err := newTree.Encode(newTreeObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree:\n%w", err)
	}

	newTreeHash, err := repo.Storer.SetEncodedObject(newTreeObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree:\n%w", err)
	}

	return newTreeHash, nil
}

// CommitMessage decorates a base commit message with the [skip changelog]
// marker when SkipChangelog is set. The marker is placed on its own line in
// the message body (rpmautospec's magic_comment_re is line-anchored).
func (c Contract) CommitMessage(base string) string {
	if !c.SkipChangelog {
		return base
	}

	return base + "\n\n" + SkipChangelogMarker
}

// LookupOverlaySidecarBlob returns the blob hash of the 'changelog' sidecar
// in the overlay tree, or plumbing.ZeroHash if no sidecar exists (e.g.,
// non-static changelog modes).
func LookupOverlaySidecarBlob(repo *gogit.Repository, overlayTreeHash plumbing.Hash) (plumbing.Hash, error) {
	tree, err := repo.TreeObject(overlayTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read overlay tree %s:\n%w", overlayTreeHash, err)
	}

	entry := findEntryByName(tree, ChangelogSidecarFilename)
	if entry == nil {
		return plumbing.ZeroHash, nil
	}

	return entry.Hash, nil
}

// --- internal helpers ---

// flipSpecBlob rewrites the spec's '%changelog' body to '%autochangelog' and
// 'Release' to '%autorelease'. Each flip is independent — a missing
// '%changelog' section does not prevent the Release flip, and a missing
// Release tag does not prevent the changelog flip.
//
// Returns the new blob hash plus a flag indicating whether the spec actually
// changed (false when the spec is malformed, both flips are no-ops, or the
// serialized output is byte-equal to the input).
func flipSpecBlob(repo *gogit.Repository, specEntry *object.TreeEntry) (plumbing.Hash, bool, error) {
	specBytes, err := readBlob(repo, specEntry.Hash)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("read blob for %#q:\n%w", specEntry.Name, err)
	}

	specFile, err := spec.OpenSpec(bytes.NewReader(specBytes))
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("malformed spec %#q: %w", specEntry.Name, err)
	}

	// Flip %changelog → %autochangelog. Missing section is non-fatal.
	if err := ReplaceChangelogBodyWithAutochangelog(specFile); err != nil {
		if !errors.Is(err, spec.ErrSectionNotFound) {
			return plumbing.ZeroHash, false, fmt.Errorf("flip %%changelog in %#q:\n%w", specEntry.Name, err)
		}
	}

	// Flip Release → %autorelease. Missing tag is non-fatal.
	if err := FlipReleaseToAutorelease(specFile); err != nil {
		if !errors.Is(err, spec.ErrNoSuchTag) {
			return plumbing.ZeroHash, false, fmt.Errorf("flip Release in %#q:\n%w", specEntry.Name, err)
		}
	}

	var buf bytes.Buffer
	if err := specFile.Serialize(&buf); err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("serialize %#q:\n%w", specEntry.Name, err)
	}

	if bytes.Equal(buf.Bytes(), specBytes) {
		return specEntry.Hash, false, nil
	}

	newHash, err := writeBlob(repo, buf.Bytes())
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("store new spec blob for %#q:\n%w", specEntry.Name, err)
	}

	return newHash, true, nil
}

// needsSidecarUpdate reports whether the tree's existing 'changelog' entry
// needs to be added or replaced to match the desired blob hash.
func needsSidecarUpdate(tree *object.Tree, desiredSidecarBlobHash plumbing.Hash) bool {
	if desiredSidecarBlobHash == plumbing.ZeroHash {
		return false
	}

	existing := findEntryByName(tree, ChangelogSidecarFilename)
	if existing == nil {
		return true
	}

	return existing.Hash != desiredSidecarBlobHash
}

// buildRewrittenEntries returns a copy of entries with the spec blob hash
// replaced (when specChanged) and the sidecar entry inserted/replaced (when
// sidecarChanged). Other entries are copied byte-for-byte. The result is
// sorted using git's tree-sort order.
func buildRewrittenEntries(
	entries []object.TreeEntry,
	specName string,
	newSpecBlobHash plumbing.Hash,
	specChanged bool,
	sidecarBlobHash plumbing.Hash,
	sidecarChanged bool,
) []object.TreeEntry {
	out := make([]object.TreeEntry, 0, len(entries)+1)
	sidecarReplaced := false

	for _, entry := range entries {
		switch entry.Name {
		case specName:
			if specChanged {
				entry.Hash = newSpecBlobHash
			}

			out = append(out, entry)
		case ChangelogSidecarFilename:
			if sidecarChanged {
				entry.Hash = sidecarBlobHash
				entry.Mode = filemode.Regular
				sidecarReplaced = true
			}

			out = append(out, entry)
		default:
			out = append(out, entry)
		}
	}

	if sidecarChanged && !sidecarReplaced {
		out = append(out, object.TreeEntry{
			Name: ChangelogSidecarFilename,
			Mode: filemode.Regular,
			Hash: sidecarBlobHash,
		})
	}

	// go-git's tree encoder requires entries in git's tree-sort order
	// (directories sort as if their name ends with '/'). Use the built-in
	// TreeEntrySorter to match git's rules exactly.
	sort.Sort(object.TreeEntrySorter(out))

	return out
}

// findEntryByName returns a pointer to the tree entry with the given name,
// or nil if none.
func findEntryByName(tree *object.Tree, name string) *object.TreeEntry {
	for i := range tree.Entries {
		if tree.Entries[i].Name == name {
			return &tree.Entries[i]
		}
	}

	return nil
}

// findTopLevelSpec returns the first file entry in tree whose name ends with
// '.spec', or nil if none is present.
func findTopLevelSpec(tree *object.Tree) *object.TreeEntry {
	for i := range tree.Entries {
		entry := &tree.Entries[i]
		if !entry.Mode.IsFile() {
			continue
		}

		if strings.HasSuffix(entry.Name, ".spec") {
			return entry
		}
	}

	return nil
}

// readBlob loads the bytes of a blob object from the repository.
func readBlob(repo *gogit.Repository, hash plumbing.Hash) ([]byte, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("open blob:\n%w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("read blob:\n%w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read blob bytes:\n%w", err)
	}

	return data, nil
}

// writeBlob writes the given bytes as a new blob object and returns its hash.
func writeBlob(repo *gogit.Repository, data []byte) (plumbing.Hash, error) {
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))

	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob writer:\n%w", err)
	}

	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()

		return plumbing.ZeroHash, fmt.Errorf("write blob bytes:\n%w", err)
	}

	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("close blob writer:\n%w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store blob:\n%w", err)
	}

	return hash, nil
}
