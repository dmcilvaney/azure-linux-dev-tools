// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
)

// ChangelogEntry is a single entry parsed from a spec's %changelog section.
//
// Upstream fedora dist-git commits frequently have bot or maintainer-of-record
// git authorship (e.g. "Fedora Release Engineering <releng@fedoraproject.org>")
// while the real authorship of each change lives in the %changelog header
// (date + name + email) and body. Synthesising commits from these entries
// preserves attribution where upstream commit metadata loses it.
type ChangelogEntry struct {
	// Header is the raw entry header line, including the leading '*'.
	// Used as the identity key when diffing two %changelog bodies.
	Header string

	// Date is the calendar date parsed from the header. Time-of-day is
	// always 00:00:00 UTC — rpm changelog headers carry day resolution only.
	Date time.Time

	// Author is the human-readable name from the header.
	Author string

	// Email is the email address from the '<...>' bracket, or "" if absent.
	Email string

	// Version is the trailing '- N-V-R' suffix from the header, or "" if absent.
	Version string

	// Body is the raw lines after the header up to (and not including) the
	// next header or end of body. Trailing blank lines are preserved
	// verbatim so callers can round-trip the entry.
	Body []string
}

// changelogHeaderRe matches an rpm %changelog header line of the form:
//
//   - Mon Jan 06 2025 Alice Real <alice@example.com> - 1.0-1
//
// Captures: month, day, year, and the trailing "who [- version]" portion.
// Weekday is consumed but not captured (it's redundant with the date).
var changelogHeaderRe = regexp.MustCompile(
	`^\*\s+\w+\s+(\w{3})\s+(\d{1,2})\s+(\d{4})\s+(.+?)\s*$`,
)

// emailSplitRe pulls a trailing "<email>" out of the "who" portion and an
// optional trailing version-release. Accepts both:
//
//   - Name <email> - 1.2.3-4   (dash before version, current Fedora style)
//   - Name <email> 1.2.3-4     (space-only before version, older Fedora style)
//   - Name <email>             (no version)
var emailSplitRe = regexp.MustCompile(`^(.*?)\s*<([^>]*)>(?:\s+(?:-\s+)?(\S.*?))?\s*$`)

// versionTailRe pulls a trailing "- version" out of the "who" portion when
// no email is present.
var versionTailRe = regexp.MustCompile(`^(.*?)\s+-\s+(\S.*)$`)

// ParseChangelogEntries parses the body lines of a spec's %changelog
// section (as returned by [ExtractStaticChangelogBody]) into structured
// entries. Entries are returned in the order they appear (newest first per
// rpm convention).
//
// Lines that look like a header but fail to parse are returned in the
// second slice so callers can decide a fallback policy. Body lines that
// precede the first header are silently dropped.
func ParseChangelogEntries(body []string) ([]ChangelogEntry, []string) {
	var (
		entries  []ChangelogEntry
		unparsed []string
		current  *ChangelogEntry
	)

	flush := func() {
		if current != nil {
			entries = append(entries, *current)
			current = nil
		}
	}

	for _, line := range body {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "*") {
			if current != nil {
				current.Body = append(current.Body, line)
			}

			continue
		}

		flush()

		entry, ok := parseHeader(line)
		if !ok {
			unparsed = append(unparsed, line)

			continue
		}

		current = &entry
	}

	flush()

	return entries, unparsed
}

// parseHeader parses a single %changelog header line into a ChangelogEntry.
// Returns ok=false if the line does not match the expected shape.
func parseHeader(line string) (ChangelogEntry, bool) {
	m := changelogHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return ChangelogEntry{}, false
	}

	mon, day, year, who := m[1], m[2], m[3], m[4]

	parsedDate, err := time.Parse("Jan 2 2006", fmt.Sprintf("%s %s %s", mon, day, year))
	if err != nil {
		return ChangelogEntry{}, false
	}

	name, email, version := splitWho(who)

	return ChangelogEntry{
		Header:  line,
		Date:    parsedDate,
		Author:  name,
		Email:   email,
		Version: version,
	}, true
}

// splitWho splits the "who" portion of a header (everything after the date)
// into name, email, and version. Any of the three may be empty.
func splitWho(who string) (name, email, version string) {
	if m := emailSplitRe.FindStringSubmatch(who); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
	}

	if m := versionTailRe.FindStringSubmatch(who); m != nil {
		return strings.TrimSpace(m[1]), "", strings.TrimSpace(m[2])
	}

	return strings.TrimSpace(who), "", ""
}

// NewChangelogEntries returns entries from current that are not present in
// parent. The result is in chronological order (oldest first), so callers
// can replay them as commits in natural order.
//
// Matching uses a normalized key (epoch timestamp + email + joined body)
// rather than the raw header line, so trivially different headers — e.g.
// `Fri Feb 06 2026` vs `Fri Feb 6 2026` — are recognised as the same entry
// and don't double-attribute. Body joins use '\n' as the separator.
//
// If parent is empty, all current entries are considered new.
func NewChangelogEntries(parent, current []ChangelogEntry) []ChangelogEntry {
	seen := make(map[string]struct{}, len(parent))
	for _, e := range parent {
		seen[normalizedKey(e)] = struct{}{}
	}

	// current is newest-first; reverse-iterate to produce oldest-first output.
	out := make([]ChangelogEntry, 0, len(current))

	for i := len(current) - 1; i >= 0; i-- {
		e := current[i]
		if _, ok := seen[normalizedKey(e)]; ok {
			continue
		}

		out = append(out, e)
	}

	return out
}

// normalizedKey produces a canonical identity for a [ChangelogEntry] so
// trivially-different header formats (date padding, name punctuation) don't
// defeat parent-vs-current dedup.
func normalizedKey(e ChangelogEntry) string {
	return fmt.Sprintf("%d|%s|%s", e.Date.Unix(), e.Email, strings.Join(e.Body, "\n"))
}

// AttributeFromTrees parses the spec's %changelog at currentTree and
// parentTree, finds entries new in current, and returns one
// [CommitMetadata] per new entry in chronological order (oldest first).
//
// Returns nil when no attribution is available: parentTree is zero (root
// commit), either tree has no spec or no %changelog section, current adds
// no new entries vs parent, OR parsing fails for ANY header in either
// tree's %changelog. Callers should treat nil as "fall back to upstream
// git metadata" — better to lose attribution than to ship a wrong author
// or over-attribute every entry because the parent failed to parse.
//
// Never returns an error — this is a best-effort attribution helper.
// Failures are debug-logged. The Hash field of returned metadata is left
// empty; callers replaying these as synth commits assign their own hashes.
//
// Picks the first top-level .spec entry in each tree (same policy as
// [Contract.Materialize] in treeflip.go). Multi-spec components fall
// back implicitly via that single-spec policy.
func AttributeFromTrees(
	repo *gogit.Repository,
	currentTree, parentTree plumbing.Hash,
) []CommitMetadata {
	if parentTree == plumbing.ZeroHash {
		// Root commit (or seed at upstream root): no parent to diff against.
		// Returning all current entries here would fan out into one synth
		// commit per pre-import %changelog entry, which is wrong.
		return nil
	}

	currentEntries, currentOK := readChangelogEntriesFromTree(repo, currentTree)
	if !currentOK {
		// Parse attempted but failed — fall back rather than risk wrong attribution.
		return nil
	}

	if len(currentEntries) == 0 {
		// No spec or no %changelog section in current — nothing to attribute.
		return nil
	}

	parentEntries, parentOK := readChangelogEntriesFromTree(repo, parentTree)
	if !parentOK {
		// Parent parse failed: we can't trust the diff. If we proceeded with
		// an empty parent we'd over-attribute every current entry as new.
		return nil
	}

	diff := NewChangelogEntries(parentEntries, currentEntries)
	if len(diff) == 0 {
		return nil
	}

	out := make([]CommitMetadata, 0, len(diff))

	for _, entry := range diff {
		message := strings.TrimRight(strings.Join(entry.Body, "\n"), "\n")
		if message == "" {
			// Body-less entry: keep the header as the message for traceability.
			message = entry.Header
		}

		out = append(out, CommitMetadata{
			Author:      entry.Author,
			AuthorEmail: entry.Email,
			Timestamp:   entry.Date.Unix(),
			Message:     message,
		})
	}

	return out
}

// readChangelogEntriesFromTree extracts the %changelog of the top-level
// .spec in tree by writing the spec blob to a temp file and invoking
// `rpmspec -q --qf ...`. librpm-canonical dates (epoch timestamps) and
// pre-extracted bodies sidestep the date-padding / multi-line-body
// pitfalls of a hand-rolled body parser.
//
// Returns:
//   - (entries, true)  on success. entries may be empty when the tree has
//     no top-level spec — a legitimate "nothing to attribute" outcome.
//   - (nil, false)     when a parse was attempted but failed: tree load
//     error, temp-file write error, or rpmspec emitted no usable entries.
//     Callers treat this as "any failure → fall back".
//
// rpmspec exits non-zero on conditions like
// `%changelog not in descending chronological order` but still writes
// partial entries to stdout. We accept whatever it emitted; the dedup
// step downstream is robust to extras and order is reconstructed from
// the epoch timestamp.
func readChangelogEntriesFromTree(repo *gogit.Repository, treeHash plumbing.Hash) ([]ChangelogEntry, bool) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		slog.Debug("changelog attribution: load tree failed", "tree", treeHash, "err", err)

		return nil, false
	}

	specEntry := findTopLevelSpec(tree)
	if specEntry == nil {
		return nil, true
	}

	data, err := readBlob(repo, specEntry.Hash)
	if err != nil {
		slog.Debug("changelog attribution: read spec blob failed", "err", err)

		return nil, false
	}

	entries, ok := parseChangelogViaRpmspec(data, specEntry.Name)
	if !ok {
		return nil, false
	}

	return entries, true
}

// rpmspecEntrySep is the sentinel rpmspec writes between %changelog
// entries. Chosen to be vanishingly unlikely in real changelog text.
const rpmspecEntrySep = "@@AZLDEV_CHANGELOG_ENTRY@@"

// parseChangelogViaRpmspec writes specBytes to a temp file and invokes
// `rpmspec -q --qf '[%{CHANGELOGTIME}|%{CHANGELOGNAME}|%{CHANGELOGTEXT}\n<sep>\n]'`
// on it, then parses the structured output into entries.
//
// The CHANGELOGNAME field is the raw "who [- version]" string per librpm;
// [splitWho] handles the name/email/version split.
//
// Returns (entries, true) when rpmspec emitted at least one parseable
// entry (even if it exited non-zero — librpm aborts on out-of-order
// changelogs but still flushes what it had). Returns (nil, false) on
// hard failures: cannot create temp file, cannot find rpmspec, or
// rpmspec produced no parseable output.
func parseChangelogViaRpmspec(specBytes []byte, specBasename string) ([]ChangelogEntry, bool) {
	tmpDir, err := os.MkdirTemp("", "azldev-attrib-*")
	if err != nil {
		slog.Debug("changelog attribution: mkdir temp failed", "err", err)

		return nil, false
	}

	defer os.RemoveAll(tmpDir)

	if specBasename == "" {
		specBasename = "spec.spec"
	}

	tmpSpec := filepath.Join(tmpDir, specBasename)
	if err := os.WriteFile(tmpSpec, specBytes, fileperms.PublicFile); err != nil {
		slog.Debug("changelog attribution: write temp spec failed", "err", err)

		return nil, false
	}

	queryFmt := "[%{CHANGELOGTIME}|%{CHANGELOGNAME}|%{CHANGELOGTEXT}\n" + rpmspecEntrySep + "\n]"

	// PROTOTYPE: ctx not threaded through AttributeFromTrees yet; use
	// Background until that refactor lands.
	cmd := exec.CommandContext(context.Background(), "rpmspec", "-q", "--qf", queryFmt, tmpSpec)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		slog.Debug("changelog attribution: rpmspec exited non-zero (using partial output)",
			"err", runErr, "stderr", stderr.String())
	}

	// rpmspec writes `error: bad date in %changelog: ...` to stderr and
	// silently drops the offending entry AND every entry below it. Entries
	// above are still emitted. Treat this as a parse failure so the caller
	// falls back instead of attributing only the head of the changelog.
	if strings.Contains(stderr.String(), "bad date in %changelog") {
		slog.Debug("changelog attribution: rpmspec dropped entries due to bad date; falling back",
			"stderr", stderr.String())

		return nil, false
	}

	entries := parseRpmspecOutput(stdout.String())
	if len(entries) == 0 {
		slog.Debug("changelog attribution: rpmspec emitted no parseable entries",
			"stderr", stderr.String())

		return nil, false
	}

	return entries, true
}

// parseRpmspecOutput parses the stdout of `rpmspec -q --qf ...` into entries.
// Each block is `<epoch>|<rawname>|<body...>` terminated by [rpmspecEntrySep].
func parseRpmspecOutput(out string) []ChangelogEntry {
	blocks := strings.Split(out, rpmspecEntrySep+"\n")
	entries := make([]ChangelogEntry, 0, len(blocks))

	for _, block := range blocks {
		block = strings.TrimSuffix(block, "\n")
		if block == "" {
			continue
		}

		const rpmspecFieldCount = 3

		parts := strings.SplitN(block, "|", rpmspecFieldCount)
		if len(parts) != rpmspecFieldCount {
			continue
		}

		ts, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			continue
		}

		date := time.Unix(ts, 0).UTC()
		rawName := strings.TrimSpace(parts[1])
		name, email, version := splitWho(rawName)

		// rpmspec emits CHANGELOGTEXT verbatim including embedded newlines
		// (multi-line bullet bodies). Split on '\n' to mirror the shape of
		// entries produced by the body-line parser.
		body := strings.Split(parts[2], "\n")

		header := fmt.Sprintf("* %s %s", date.Format("Mon Jan 02 2006"), rawName)

		entries = append(entries, ChangelogEntry{
			Header:  header,
			Date:    date,
			Author:  name,
			Email:   email,
			Version: version,
			Body:    body,
		})
	}

	return entries
}
