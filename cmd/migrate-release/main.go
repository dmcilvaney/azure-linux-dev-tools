// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command migrate-release computes release offset bumps for components
// transitioning from static Release tags to %autorelease.
//
// Prerequisites:
//  1. Render all specs with the OLD azldev binary into a git-tracked output
//     directory and commit the result.
//  2. Render all specs with the NEW azldev binary (autorelease flip) into the
//     same directory (overwrites working tree, HEAD still has old renders).
//
// The tool then reads each component's spec from HEAD (old Release value) and
// from the working tree (new release_number from rpmautospec header), computes
// the gap, and writes bumps entries to the lock files.
//
// Delete this tool after migration is complete.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/lockfile"
	"github.com/spf13/afero"
)

// staticReleaseRE extracts the integer from a static Release tag line.
var staticReleaseRE = regexp.MustCompile(`(?i)^Release:\s+(\d+)`)

// releaseNumberRE extracts the release_number from rpmautospec's generated header.
// Matches: "    release_number = 35;"
var releaseNumberRE = regexp.MustCompile(`^\s*release_number\s*=\s*(\d+)\s*;`)

func main() {
	projectDir := flag.String("C", ".", "project directory (git repo root)")
	specsSubdir := flag.String("specs-subdir", "specs", "subdirectory within project containing rendered specs")
	dryRun := flag.Bool("dry-run", false, "print proposed changes without writing lock files")
	componentFilter := flag.String("component", "", "process only this component (default: all)")
	flag.Parse()

	lockDir := filepath.Join(*projectDir, "locks")
	osFS := afero.NewOsFs()

	// Open the project repo to read old specs from HEAD.
	specsRepo, err := gogit.PlainOpen(*projectDir)
	if err != nil {
		log.Fatalf("opening git repo at %q: %v", *projectDir, err)
	}

	headRef, err := specsRepo.Head()
	if err != nil {
		log.Fatalf("reading HEAD of specs repo: %v", err)
	}

	headCommit, err := specsRepo.CommitObject(headRef.Hash())
	if err != nil {
		log.Fatalf("reading HEAD commit: %v", err)
	}

	headTree, err := headCommit.Tree()
	if err != nil {
		log.Fatalf("reading HEAD tree: %v", err)
	}

	entries, err := afero.ReadDir(osFS, lockDir)
	if err != nil {
		log.Fatalf("reading lock dir %q: %v", lockDir, err)
	}

	var stats struct {
		total, skipped, bumped, noBump, errored int
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".lock")
		if *componentFilter != "" && name != *componentFilter {
			continue
		}

		stats.total++

		lockPath := filepath.Join(lockDir, entry.Name())

		lock, loadErr := lockfile.Load(osFS, lockPath)
		if loadErr != nil {
			log.Printf("ERROR %s: loading lock: %v", name, loadErr)
			stats.errored++

			continue
		}

		// Skip local components.
		if lock.UpstreamCommit == "" {
			log.Printf("SKIP  %s: local component", name)
			stats.skipped++

			continue
		}

		// Skip if already has bumps.
		if len(lock.Bumps) > 0 {
			log.Printf("SKIP  %s: already has bumps entries", name)
			stats.skipped++

			continue
		}

		// Spec path in repo: <specs-subdir>/<first-letter>/<name>/<name>.spec
		letter := strings.ToLower(name[:1])
		specRelPath := filepath.Join(*specsSubdir, letter, name, name+".spec")

		// Read old Release from HEAD.
		oldRelease, oldErr := readOldRelease(specsRepo, headTree, specRelPath)
		if oldErr != nil {
			log.Printf("ERROR %s: reading old Release from HEAD: %v", name, oldErr)
			stats.errored++

			continue
		}

		// Read new release_number from working tree.
		newSpecPath := filepath.Join(*projectDir, specRelPath)

		newRelease, newErr := readNewReleaseNumber(newSpecPath)
		if newErr != nil {
			log.Printf("ERROR %s: reading new release_number: %v", name, newErr)
			stats.errored++

			continue
		}

		gap := oldRelease - newRelease

		if gap <= 0 {
			log.Printf("OK    %-30s  old=%d  new=%d  gap=%d  (no bump needed)",
				name, oldRelease, newRelease, gap)
			stats.noBump++

			continue
		}

		log.Printf("BUMP  %-30s  old=%d  new=%d  gap=%d  anchor=%s",
			name, oldRelease, newRelease, gap, lock.UpstreamCommit[:7])

		if !*dryRun {
			lock.Bumps = map[string]int{lock.UpstreamCommit: gap}

			if saveErr := lock.Save(osFS, lockPath); saveErr != nil {
				log.Printf("ERROR %s: writing lock: %v", name, saveErr)
				stats.errored++

				continue
			}

			// Add a comment explaining the bump entry.
			comment := fmt.Sprintf(
				"# Migration: static Release %d -> %%autorelease (base %d) at upstream commit %s. Gap = %d.\n",
				oldRelease, newRelease, lock.UpstreamCommit[:12], gap)

			if commentErr := insertCommentAboveBumps(lockPath, comment); commentErr != nil {
				log.Printf("WARN  %s: could not add comment to lock: %v", name, commentErr)
			}
		}

		stats.bumped++
	}

	log.Printf("\nSummary: total=%d bumped=%d no-bump=%d skipped=%d errored=%d",
		stats.total, stats.bumped, stats.noBump, stats.skipped, stats.errored)

	if *dryRun && stats.bumped > 0 {
		log.Print("(dry-run: no lock files were modified)")
	}

	if stats.errored > 0 {
		os.Exit(1)
	}
}

// readOldRelease reads the static Release value from the spec at HEAD in the
// rendered-specs repo. Returns the integer portion (e.g., 52 from "52%{?dist}").
func readOldRelease(repo *gogit.Repository, headTree *object.Tree, specRelPath string) (int, error) {
	// Navigate the tree to find the spec blob.
	treeEntry, err := headTree.FindEntry(specRelPath)
	if err != nil {
		// Try with forward slashes (go-git uses / internally).
		specRelPath = strings.ReplaceAll(specRelPath, string(filepath.Separator), "/")

		treeEntry, err = headTree.FindEntry(specRelPath)
		if err != nil {
			return 0, fmt.Errorf("spec %q not found in HEAD tree: %w", specRelPath, err)
		}
	}

	blob, err := repo.BlobObject(treeEntry.Hash)
	if err != nil {
		return 0, fmt.Errorf("reading spec blob: %w", err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return 0, fmt.Errorf("opening spec blob reader: %w", err)
	}
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if matches := staticReleaseRE.FindStringSubmatch(line); matches != nil {
			value, parseErr := strconv.Atoi(matches[1])
			if parseErr != nil {
				return 0, fmt.Errorf("parsing Release int from %q: %w", line, parseErr)
			}

			return value, nil
		}
	}

	return 0, fmt.Errorf("no static Release tag found in spec")
}

// readNewReleaseNumber reads the release_number from the rpmautospec header
// in the working-tree spec file. This is the value rpmautospec computed for
// %autorelease, before any bumps.
func readNewReleaseNumber(specPath string) (int, error) {
	file, err := os.Open(specPath)
	if err != nil {
		return 0, fmt.Errorf("opening spec %q: %w", specPath, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if matches := releaseNumberRE.FindStringSubmatch(line); matches != nil {
			value, parseErr := strconv.Atoi(matches[1])
			if parseErr != nil {
				return 0, fmt.Errorf("parsing release_number from %q: %w", line, parseErr)
			}

			return value, nil
		}
	}

	return 0, fmt.Errorf("no release_number found in rpmautospec header")
}

// insertCommentAboveBumps reads a lock file, finds the [bumps] section header,
// and inserts a comment line above it explaining why the entry exists.
func insertCommentAboveBumps(lockPath, comment string) error {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")

	var result []string

	for _, line := range lines {
		if strings.TrimSpace(line) == "[bumps]" {
			result = append(result, comment+"[bumps]")
		} else {
			result = append(result, line)
		}
	}

	return os.WriteFile(lockPath, []byte(strings.Join(result, "\n")), 0o644)
}
