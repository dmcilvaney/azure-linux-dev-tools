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

// flipUpstreamSpecChangelogInTree returns a tree hash equivalent to the given
// upstream tree, except that:
//   - the top-level '*.spec' file's '%changelog' body is rewritten to a single
//     '%autochangelog' line; and
//   - when sidecarBlobHash is non-zero, a top-level 'changelog' entry is
//     added (or replaced) pointing at that blob.
//
// This is used when replaying intermediate upstream commits in the synthetic
// dist-git so that rpmautospec sees an unbroken chain from HEAD back to the
// kept import-commit on BOTH of its boundary checks:
//   - the spec body must remain '%autochangelog' throughout (so the
//     autochangelog walk does not terminate early); and
//   - the 'changelog' sidecar file must be present in every commit above the
//     seed (so the sidecar-boundary check does not stop propagation).
//
// When sidecarBlobHash is plumbing.ZeroHash, no sidecar is injected — used
// for non-static changelog modes that do not produce a sidecar file.
//
// Returns the original tree hash unchanged when:
//   - the tree has no '*.spec' at the top level;
//   - the spec cannot be parsed;
//   - the spec has no '%changelog' section; and
//   - the spec body flip is a no-op AND the sidecar is already present with
//     the desired blob hash (or no sidecar was requested).
func flipUpstreamSpecChangelogInTree(
	repo *gogit.Repository,
	treeHash plumbing.Hash,
	sidecarBlobHash plumbing.Hash,
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

	sidecarChanged := needsSidecarUpdate(tree, sidecarBlobHash)

	if !specChanged && !sidecarChanged {
		return treeHash, nil
	}

	newEntries := buildRewrittenEntries(tree.Entries, specEntry.Name,
		newSpecBlobHash, specChanged, sidecarBlobHash, sidecarChanged)

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

// flipSpecBlob rewrites the spec's '%changelog' body to '%autochangelog' and
// returns the new blob hash plus a flag indicating whether the spec actually
// changed (false when the spec is malformed, has no '%changelog', or the
// flip is a no-op).
func flipSpecBlob(repo *gogit.Repository, specEntry *object.TreeEntry) (plumbing.Hash, bool, error) {
	specBytes, err := readBlob(repo, specEntry.Hash)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("read blob for %#q:\n%w", specEntry.Name, err)
	}

	specFile, err := spec.OpenSpec(bytes.NewReader(specBytes))
	if err != nil {
		// Malformed spec — leave it alone rather than failing the render.
		return specEntry.Hash, false, nil //nolint:nilerr // graceful degradation
	}

	if err := ReplaceChangelogBodyWithAutochangelog(specFile); err != nil {
		if errors.Is(err, spec.ErrSectionNotFound) {
			return specEntry.Hash, false, nil
		}

		return plumbing.ZeroHash, false, fmt.Errorf("flip %%changelog in %#q:\n%w", specEntry.Name, err)
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
// (if any) needs to be added/replaced/removed to match the desired blob hash.
// ZeroHash means "no sidecar wanted".
func needsSidecarUpdate(tree *object.Tree, desiredSidecarBlobHash plumbing.Hash) bool {
	existing := findEntryByName(tree, ChangelogSidecarFilename)

	if desiredSidecarBlobHash == plumbing.ZeroHash {
		// We don't want a sidecar. Only an update is needed if one already
		// exists (and it would need to be dropped). But we never drop —
		// non-static modes never call this path with a populated overlay
		// sidecar, and a stray sidecar in upstream content is unexpected.
		// Treat as "no change needed".
		return false
	}

	if existing == nil {
		return true
	}

	return existing.Hash != desiredSidecarBlobHash
}

// buildRewrittenEntries returns a copy of entries with the spec entry's hash
// replaced (when specChanged) and the sidecar entry inserted/replaced (when
// sidecarChanged). Other entries are copied byte-for-byte.
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

	// go-git's tree encoder requires entries sorted by name.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})

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

// lookupOverlaySidecarBlob returns the blob hash of the 'changelog' sidecar
// in the overlay tree, or plumbing.ZeroHash if no sidecar exists (e.g.,
// non-static changelog modes).
func lookupOverlaySidecarBlob(repo *gogit.Repository, overlayTreeHash plumbing.Hash) (plumbing.Hash, error) {
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
