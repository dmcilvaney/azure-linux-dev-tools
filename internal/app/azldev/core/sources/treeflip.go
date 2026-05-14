// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
)

// flipUpstreamSpecChangelogInTree returns a tree hash equivalent to the given
// upstream tree, except that the top-level '*.spec' file's '%changelog' body
// is rewritten to a single '%autochangelog' line. No 'changelog' sidecar is
// written into the tree — rpmautospec only reads the sidecar at process-distgit
// time (i.e., from HEAD), not from each historical commit.
//
// This is used when replaying intermediate upstream commits in the synthetic
// dist-git so that rpmautospec sees an unbroken '%autochangelog' chain from
// HEAD back to the kept import-commit (which retains the original static
// body and acts as the seed where the rpmautospec walk terminates).
//
// Returns the original tree hash unchanged when:
//   - the tree has no '*.spec' at the top level;
//   - the spec cannot be parsed;
//   - the spec has no '%changelog' section;
//   - the spec's '%changelog' body is already '%autochangelog'.
func flipUpstreamSpecChangelogInTree(repo *gogit.Repository, treeHash plumbing.Hash) (plumbing.Hash, error) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read tree %s:\n%w", treeHash, err)
	}

	specEntry := findTopLevelSpec(tree)
	if specEntry == nil {
		return treeHash, nil
	}

	specBytes, err := readBlob(repo, specEntry.Hash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read blob for %#q:\n%w", specEntry.Name, err)
	}

	specFile, err := spec.OpenSpec(bytes.NewReader(specBytes))
	if err != nil {
		// Malformed spec — leave the tree alone rather than failing the whole render.
		return treeHash, nil //nolint:nilerr // graceful degradation
	}

	if err := ReplaceChangelogBodyWithAutochangelog(specFile); err != nil {
		if errors.Is(err, spec.ErrSectionNotFound) {
			return treeHash, nil
		}

		return plumbing.ZeroHash, fmt.Errorf("flip %%changelog in %#q:\n%w", specEntry.Name, err)
	}

	var buf bytes.Buffer
	if err := specFile.Serialize(&buf); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("serialize %#q:\n%w", specEntry.Name, err)
	}

	if bytes.Equal(buf.Bytes(), specBytes) {
		return treeHash, nil
	}

	newBlobHash, err := writeBlob(repo, buf.Bytes())
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store new spec blob for %#q:\n%w", specEntry.Name, err)
	}

	newEntries := make([]object.TreeEntry, len(tree.Entries))
	copy(newEntries, tree.Entries)

	for i := range newEntries {
		if newEntries[i].Name == specEntry.Name {
			newEntries[i].Hash = newBlobHash
		}
	}

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
