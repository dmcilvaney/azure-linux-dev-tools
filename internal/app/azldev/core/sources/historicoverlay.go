// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"bytes"
	"log/slog"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/rpm/spec"
	"github.com/samber/lo"
)

// applyHistoricSpecOverlays returns a tree hash with the component's resolved
// spec-modifying overlays applied to the tree's top-level spec.
//
// It is best-effort and atomic per commit: if the tree has no spec, the spec is
// malformed, or no overlay produces any change, the input tree hash is returned
// unchanged so the commit's files are left as-is (and the caller moves on to the
// next commit). Individual overlays whose anchors are absent at this point in
// history are skipped while the rest still apply. Only the spec blob is
// rewritten; non-spec side-effects of overlays (e.g. copying a patch file into
// sources) are intentionally ignored, since they do not affect the
// version/release/changelog that rpmautospec attributes to the commit.
func applyHistoricSpecOverlays(
	repo *gogit.Repository, treeHash plumbing.Hash, overlays []projectconfig.ComponentOverlay,
) plumbing.Hash {
	specOverlays := lo.Filter(overlays, func(o projectconfig.ComponentOverlay, _ int) bool {
		return o.ModifiesSpec()
	})
	if len(specOverlays) == 0 {
		return treeHash
	}

	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		slog.Debug("historic overlays: failed to read tree; leaving commit as-is", "tree", treeHash, "err", err)

		return treeHash
	}

	specEntry := findTopLevelSpec(tree)
	if specEntry == nil {
		return treeHash
	}

	specBytes, err := readBlob(repo, specEntry.Hash)
	if err != nil {
		slog.Debug("historic overlays: failed to read spec blob; leaving commit as-is",
			"spec", specEntry.Name, "err", err)

		return treeHash
	}

	newSpecBytes, changed := rewriteSpecWithOverlays(specBytes, specOverlays, specEntry.Name)
	if !changed {
		return treeHash
	}

	newSpecHash, err := writeBlob(repo, newSpecBytes)
	if err != nil {
		slog.Debug("historic overlays: failed to store spec blob; leaving commit as-is",
			"spec", specEntry.Name, "err", err)

		return treeHash
	}

	newEntries := buildRewrittenEntries(tree.Entries, specEntry.Name, newSpecHash, true, plumbing.ZeroHash, false)

	newTree := &object.Tree{Entries: newEntries}
	newTreeObj := repo.Storer.NewEncodedObject()

	if err := newTree.Encode(newTreeObj); err != nil {
		slog.Debug("historic overlays: failed to encode tree; leaving commit as-is", "err", err)

		return treeHash
	}

	newTreeHash, err := repo.Storer.SetEncodedObject(newTreeObj)
	if err != nil {
		slog.Debug("historic overlays: failed to store tree; leaving commit as-is", "err", err)

		return treeHash
	}

	return newTreeHash
}

// rewriteSpecWithOverlays applies the spec-modifying overlays to specBytes and
// returns the serialized result. Overlays are applied best-effort and
// independently: an overlay whose anchor is absent at this point in history
// (e.g. a `%setup` line that upstream phrased differently) is skipped, and the
// remaining overlays still apply. This matters because the version-setting
// overlays (e.g. `%define specversion`) must not be abandoned just because an
// unrelated structural overlay failed to match the older spec.
//
// The bool is false (and the returned bytes nil) when the spec is malformed,
// serialization fails, or no overlay produced any change — signalling the
// caller to leave the commit's tree untouched.
func rewriteSpecWithOverlays(
	specBytes []byte, specOverlays []projectconfig.ComponentOverlay, specName string,
) ([]byte, bool) {
	specFile, err := spec.OpenSpec(bytes.NewReader(specBytes))
	if err != nil {
		slog.Debug("historic overlays: malformed spec; leaving commit as-is", "spec", specName, "err", err)

		return nil, false
	}

	for _, overlay := range specOverlays {
		if applyErr := ApplySpecOverlay(overlay, specFile); applyErr != nil {
			slog.Debug("historic overlays: overlay did not apply; skipping just this overlay",
				"spec", specName, "type", overlay.Type, "err", applyErr)

			continue
		}
	}

	var buf bytes.Buffer
	if err := specFile.Serialize(&buf); err != nil {
		slog.Debug("historic overlays: failed to serialize spec; leaving commit as-is",
			"spec", specName, "err", err)

		return nil, false
	}

	if bytes.Equal(buf.Bytes(), specBytes) {
		return nil, false
	}

	return buf.Bytes(), true
}
