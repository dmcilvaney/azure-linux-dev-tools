# PRD: Lock File–Based Release Tracking

**Related ideas:** bn-c5ae (Component change detection for build queue determination), bn-8bb7 (Compute full NEVR for all component subpackages)

## Problem

azldev currently tracks release bump counts via `Affects: <component>` trailer lines in git commit messages. The system walks the project repo's git log, counts matching commits, and feeds that count into:

1. **Fingerprinting** — `AffectsCommitCount` is a field in `ComponentInputs`, fed into `combineInputs()` to produce the SHA256 fingerprint. A different count → different fingerprint → component marked as changed.
2. **Synthetic history** — `buildSyntheticCommits()` finds all `Affects:` commits and `CommitSyntheticHistory()` replays them as synthetic empty commits on the prepared dist-git so rpmautospec computes the correct `%autorelease` release number.

### Issues with the Current Approach

- **Fragile**: Developers must remember to add trailer lines to commit messages with exact formatting. Easy to forget, typo, or get wrong.
- **Non-obvious**: Most packaging ecosystems use files (lock files, changelogs, `.spec` Release tags) to track release state — not commit message trailers. The `Affects:` convention surprises developers.
- **Hard to audit**: Release state is smeared across potentially hundreds of commits. To know "how many times has curl been bumped?", you must walk the full git log. There is no single source of truth visible in the repo tree.
- **Brittle across rebases/squashes**: Any rebase, squash, cherry-pick, or history rewrite can silently change the count, altering fingerprints and release numbers with no visible diff.
- **Slow for large repos**: `FindAffectsCommits` walks the entire commit history for every component during identity computation (and again during source preparation).
- **Non-deterministic upstream resolution**: For upstream (Fedora) components, `resolveEffectiveCommitHash()` resolves a snapshot timestamp to the "last commit before date" via `GetCommitHashBeforeDate()`. If an upstream PR is merged with a backdated author timestamp, the resolved commit silently changes even though nothing in the azldev project changed. There is no way to detect this has happened without re-running identity resolution.

## Proposal

Replace `Affects:` commit counting with a **lock file** that explicitly records each component's release bump count, resolved upstream commit hash, and identity fingerprint. The lock file is a tracked, diffable TOML file — changes are visible in PRs as normal file diffs.

### Key Benefits

#### Reduce human error

The `Affects:` commit trailer is easy to forget, typo, or get wrong. The lock file replaces this with tooling-managed state — `bump` and `update` do the bookkeeping. Release strategy awareness (`auto`/`explicit`/`manual`) means components get the right behavior automatically, and misconfigurations (e.g., two sources of truth for `Release:`) are caught as errors, not silent bugs.

#### Reduce manual toil

Moving the snapshot forward currently requires manually counting and creating `Affects:` commits for every changed package. With `azldev component update -a`, a single command re-resolves all upstream commits, detects what changed via fingerprint comparison, and auto-bumps — handling hundreds of components in seconds. The `bump`/`update` cycle replaces a multi-step, error-prone manual process.

#### Enable rendered spec output

Because the lock file deterministically captures all inputs (upstream commit, release count, overlays), the resolved state of every component becomes a pure function of the lock file + config. Updating a **rendered spec output** can become a natural extension of the lock file workflow: `azldev component update` can regenerate the rendered spec files based on the updated lock file, or `azldev component render -a` can be used to explicitly render the specs at any time.

Rendered spec output brings several benefits:

- **PR reviewability** — reviewers see the actual spec that will be built, not a mental stack of overlays. The PR diff shows exactly what changed in the final spec.
- **Auditability** — a stable, checked-in record of what was built for each release. Provides a stand-in for the lost `Affects:` commit history.
- **Customer friendliness** — distro users are accustomed to seeing the spec files in the repository. Rendered spec output ensures they can easily inspect what will be built without needing to understand the overlay system.

Spec rendering is a direct follow-on to this PRD. The lock file design is a prerequisite: without deterministic, locked inputs, rendering would produce non-reproducible output.

## Design

### Release Strategies

Three distinct release strategies must be supported. The lock file interacts with each differently.

#### Case 1: Auto-release packages (upstream-derived)

These packages use `%autorelease` from upstream. rpmautospec counts commits in the dist-git to compute the release number.

**Lock file role**: The `release` count drives synthetic history generation. `azldev component bump curl` increments the count; `CommitSyntheticHistory()` creates that many synthetic commits so rpmautospec computes the correct `%autorelease` value.

This is the PRD's primary scenario. No config changes needed — auto-release is the default behavior.

#### Case 2: Explicit numeric release packages

These packages have a hardcoded `Release:` tag (e.g., `Release: 5%{?dist}`). rpmautospec is not involved — the release number is whatever the spec says.

**Lock file role**: The lock file stores a `release` value that represents the **absolute release number**. During source preparation, the `release` value is applied as an implicit `spec-set-tag` overlay that sets the spec's `Release:` tag — this overlay is injected at build time and never written to config files.

This approach avoids the need for TOML round-trip editing infrastructure and keeps config files human-only. The lock file diff in PRs shows exactly what changed. `azldev component bump` increments the `release` counter in the lock file, same as for `auto` strategy.

Only simple `Release:` formats are supported for `explicit` strategy: `N` or `N%{?dist}`. If the upstream spec uses complex macros in its `Release:` tag, use `manual` strategy instead.

#### Case 3: Fully manual / complex packages (e.g., kernel)

These packages are too complex for automated release management. The maintainer explicitly manages the release via TOML config, overlays, and/or macros.

**Lock file role**: These packages have **no `release` field** in the lock file. The `upstream-commit` pinning and change detection still apply — the lock file tracks *what* changed, but the maintainer decides *when* to bump and *how* to set the release. `azldev component bump` is an **error** for `manual` packages. `update` skips the auto-bump step for these and logs a warning.

#### Config: `release-strategy` field

To distinguish these cases, add a `release-strategy` field to `ComponentBuildConfig`:

```toml
[components.curl]
spec.type = "upstream"

[components.curl.build]
release-strategy = "auto"  # default — uses %autorelease via synthetic history

[components.glibc]
spec.type = "upstream"

[components.glibc.build]
release-strategy = "explicit"  # Release: tag is hardcoded in spec

[components.kernel]
spec.type = "upstream"

[components.kernel.build]
release-strategy = "manual"  # fully manual — skip auto-bumping entirely
```

| Strategy   | Lock `release` field? | Consumed as                                       | `bump` behavior                | `update` auto-bump? |
|------------|----------------------|---------------------------------------------------|--------------------------------|---------------------|
| `auto`     | Yes                  | Synthetic commit count (drives `%autorelease`)     | Increments lock file `release` | Yes                 |
| `explicit` | Yes                  | Implicit `spec-set-tag Release:` at build time     | Increments lock file `release` | Yes                 |
| `manual`   | No                   | N/A                                                | Error                          | Skipped             |

Default is `auto` (most common case). `bump` is uniform for `auto` and `explicit` — always "increment a number in the lock file." The strategy only affects how the value is *consumed* at build time. Tools never modify config files; only the lock file is written.

### File Format

TOML, matching the existing project config conventions. One entry per component that has been bumped or locked at least once.

```toml
# azldev.lock — Component release and source tracking
# Managed by `azldev component bump` and `azldev component update`.
# Do not edit manually unless you know what you're doing.
version = 1

[curl]
release = 3
upstream-commit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
fingerprint = "sha256:abcdef..."

[kernel]
# no release field — manual strategy, maintainer manages Release: tag
upstream-commit = "f6e5d4c3b2a1f6e5d4c3b2a1f6e5d4c3b2a1f6e5"
fingerprint = "sha256:123456..."

[bash]
release = 1
fingerprint = "sha256:fedcba..."
```

The `upstream-commit` field records the resolved git commit hash for upstream components — always stored as full 40-character hex hashes for unambiguous identity (short hashes are accepted in TOML config but resolved to full length by `update`). Components with `spec.type = "local"` do not have this field.

#### Fingerprint Tracking

```toml
[curl]
release = 3
upstream-commit = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
fingerprint = "sha256:abcdef..."  # identity fingerprint at time of last bump/update
```

The `fingerprint` field records the component's identity fingerprint at the time of the last `update` or `bump`. This enables `azldev` to detect when a component's inputs have changed (by recomputing the fingerprint and comparing to the stored value), which drives the auto-bump behavior of `update` and CI validation ("did you forget to bump?"). Both `bump` and `update` recompute and store the fingerprint after any mutation.

### File Layout

A single `azldev.lock` file at the project root, alongside `azldev.toml`. All components are tracked in this one file. Since only tooling writes to the lock file (never humans), a single file is simpler — no discovery logic, no ambiguity about which lock file owns a component.

#### Missing Lock Entry Behavior

| Component type | Missing `upstream-commit` | Missing `release` | `bump`/`update` work? |
|---------------|--------------------------|-------------------|----------------------|
| `upstream`    | Error: run `component update` | Default to `0` | Yes (`auto`/`explicit`) |
| `local`       | N/A (field doesn't apply) | Default to `0` | Yes (`auto`/`explicit`) |

When no lock file exists at all, commands that read it (`identity`, `build`, `prepare-sources`) produce an actionable error: *"No lock file found. Run `azldev component update -a` to initialize."*

### Upstream Commit Pinning

#### The Problem

Today, upstream commit resolution follows a priority chain in `resolveEffectiveCommitHash()`:

1. **Explicit `upstream-commit`** in the component's TOML config → deterministic
2. **Snapshot timestamp** on the distro reference → **non-deterministic** (a backdated upstream merge changes the resolved commit)
3. **HEAD** → non-deterministic (changes on every upstream push)

Cases 2 and 3 mean that the same azldev project can resolve to different upstream source commits depending on *when* you run the build — even with zero changes to the project itself. This is a reproducibility problem.

#### Solution: Lock the Resolved Commit

The lock file is the **single source of truth** for upstream commit resolution at build time. For any upstream component, the lock file's `upstream-commit` is what gets used — period. There is no priority chain or fallback.

- **No lock entry** → config error: *"no lock entry for curl, run `azldev component update curl`"*
- **`spec.upstream-commit` in TOML doesn't match lock file** → config error: *"lock file is stale for curl (config says abc123, lock says def456), run `azldev component update curl`"*. This check only applies when `spec.upstream-commit` is explicitly set in the TOML. When unset (most packages use snapshots), the lock file's `upstream-commit` is trusted as-is.
- **Lock entry present and consistent** → use it. No clone, no network access.

The `spec.upstream-commit` field in the TOML config becomes an **input constraint** for the `update` command, not a runtime override. When you run `azldev component update curl` and the component has `spec.upstream-commit = "abc123"`, the update command resolves that commit and writes it to the lock file. At build time, the lock file is what's read.

This matches the `go.mod`/`go.sum` model: `go build` errors if `go.sum` is stale relative to `go.mod`. Developers already expect this behavior from the analogy we're drawing.

This is directly analogous to how `Cargo.lock` pins resolved crate versions and `go.sum` pins resolved module checksums — the lock file captures the result of a non-deterministic resolution so that subsequent builds are reproducible.

From a developer's perspective: the distro `.toml` config files are like `go.mod` or `Cargo.toml` — they declare *what* you want (which upstream distro, which snapshot window, which overlays). The `.lock` file is like `go.sum` or `Cargo.lock` — it records *exactly what was resolved* (the specific commit hash, the current release count). You edit the `.toml`; tooling updates the `.lock`.

#### `azldev component update [-a | <component>...]`

Re-resolves upstream commits and auto-bumps any component whose fingerprint changed. Uses the standard component selector (`-a`/`--all-components` for all, or named components). This is the primary workflow command — it handles the full "re-lock and bump" cycle in one step.

```bash
# Example: moving the snapshot forward
# 1. Update the distro config
$ vim distro.toml  # snapshot = "2026-03-01" → "2026-04-01"

# 2. Re-resolve upstream commits and auto-bump changed components (-a = all)
$ azldev component update -a
curl:    upstream-commit a1b2c3d4 → f9e8d7c6, release 3 → 4
bash:    unchanged
kernel:  upstream-commit d4c3b2a1 → e5f6a7b8 (manual strategy — bump skipped)
glibc:   unchanged
openssl: upstream-commit c1d2e3f4 → a9b8c7d6, release 2 → 3
python3: upstream-commit e5f6a7b8 → b1c2d3e4, Release 12%{?dist} → 13%{?dist} (overlay updated)
Updated: 200 components, bumped: 195, skipped: 5 (manual)

# 3. Commit
$ git add -A && git commit -m "chore: move snapshot to 2026-04-01"

# Update a single component
$ azldev component update curl
curl: upstream-commit a1b2c3d4 → f9e8d7c6, release 3 → 4

# Re-resolve without bumping (inspect-only)
$ azldev component update -a --no-bump
curl: upstream-commit a1b2c3d4 → f9e8d7c6 (bump skipped)
```

The `update` command:

1. Re-resolves upstream commits via snapshot time, HEAD, or explicit pin (the existing `resolveEffectiveCommitHash` logic).
2. Writes the resolved hash to the lock file.
3. Recomputes each component's fingerprint and compares to the stored `fingerprint` in the lock file.
4. For any component whose fingerprint changed, auto-bumps according to `release-strategy`:
   - `auto` → increment lock file `release` counter.
   - `explicit` → increment lock file `release` counter (applied as implicit `spec-set-tag` overlay at build time).
   - `manual` → skip bump, log a warning that the component needs manual attention.
5. Updates the stored `fingerprint` in the lock file.

The `--no-bump` flag skips step 4 — useful for inspecting what changed before committing. `manual` strategy components always behave as if `--no-bump` was passed for the bump step.

`update` prints a summary of changes (component → old/new commit, bumped/skipped, total counts) as it processes, providing visibility into what's happening. This is analogous to `go get -u` printing what it updates.

#### Integration with Source Provider

`FedoraSourcesProviderImpl` no longer has a priority chain for commit resolution. At build time:

1. **Read lock file** — if `upstream-commit` is present, use it.
2. **No lock entry** → error. The developer must run `azldev component update` first.
3. **Lock entry doesn't match `spec.upstream-commit`** (if set in TOML) → error. The lock file is stale.

The existing `resolveEffectiveCommitHash()` logic (snapshot time, HEAD, explicit commit) is preserved but **only used by the `update` command** to resolve commits. It is no longer called during builds or identity resolution.

For `ResolveSourceIdentity()`, the function reads the lock file and returns immediately — no clone, no network access. This makes identity resolution faster and deterministic.

For `GetComponent()` (actual source fetch), the locked commit is used for checkout.

### Changelog Generation

#### Approach: Git Log of Lock File + Preserved Upstream History

Changelog entries are derived from two sources, with no new data entry required:

1. **Upstream history** — for upstream components, the dist-git's `.git` directory is preserved during source preparation (`WithPreserveGitDir()`). The upstream commit log carries real author names, dates, and messages from Fedora/upstream maintainers. These form the base of the changelog.

2. **Project repo git log** — every `bump` or `update` that changes a component's lock entry creates a git commit in the azldev project repo. At source-prep time, `azldev` walks `git log -- azldev.lock`, extracts commits that changed the component's `release` or `fingerprint` fields, and uses those commit messages as changelog entries. Since developers already write meaningful commit messages ("fix(curl): add CVE-2025-1234 patch", "chore: move snapshot to 2026-04-01"), no additional input is needed.

The resulting dist-git has our commits appended on top of the upstream history:

```text
A3  fix(curl): add CVE-2025-1234 patch           ← our release 3
A2  feat(curl): enable HTTP/3 support              ← our release 2
A1  chore: initial Azure Linux build               ← baseline (overlay changes)
U4  Update to 8.7.1                                ← upstream
U3  Fix test failures on s390x                     ← upstream
U2  Update to 8.6.0                                ← upstream
```

rpmautospec's `%autochangelog` reads this top-down and produces properly interleaved entries with our changes appearing above the upstream history — which is the correct chronological ordering.

#### Per-strategy behavior

- **`auto`** (`%autorelease` + `%autochangelog`): Upstream git history preserved, our changelog entries appended as synthetic commits. rpmautospec handles both release numbering and changelog generation. `release` in the lock file means "number of additional bumps on top of upstream" — the total `%autorelease` value is `upstream_commit_count + release + 1`.

- **`explicit`** (`Release: N` + `%changelog`): During source preparation, `rpmdev-bumpspec` is used to both set the `Release:` tag and add `%changelog` entries. For each changelog entry (from the git log of the lock file), `rpmdev-bumpspec -c "message"` is invoked, which handles `Release:` format parsing and `%changelog` formatting natively. This eliminates the need for azldev to parse `Release:` formats — `rpmdev-bumpspec` handles `N`, `N%{?dist}`, and more complex formats.

- **`explicit`** (`Release: N` + `%autochangelog`): Same as `auto` for changelog — upstream history + synthetic commits. The implicit `Release:` overlay still applies the lock file's value.

- **`manual`**: No automated changelog generation. The maintainer manages the changelog manually.

#### Release/commit-count mismatch

The lock file `release` count is the source of truth for the release number. The number of git commits that touched the lock file for this component may differ (squashes, amends, batch updates). The reconciliation is:

- **Use `release` for the count** — it determines the number of synthetic commits (for `auto`) or the value passed to `rpmdev-bumpspec` (for `explicit`)
- **Use git log for the messages** — best effort. Map the most recent N messages to the N synthetic commits. If fewer messages than releases: pad with generic entries. If more messages than releases: use the latest N.

Imperfect changelog messages are acceptable; incorrect release numbers are not.

#### Implications

- **No new data entry** — commit messages and upstream git history already exist.
- **Per-release granular** — each commit that bumps a component produces a separate changelog entry, unlike the previous overlay-description approach which was snapshot-only.
- **Release never resets** — `release` is monotonically increasing ("total times bumped, ever"). No reset on version change. This simplifies the model and avoids "when do we reset?" ambiguity. `curl-8.7.1-5` → `curl-8.8.0-6`, not `curl-8.8.0-1`.
- **`rpmdev-bumpspec` dependency** — required in the build environment for `explicit` strategy components. Already available in mock environments.
- **`last-bumped` timestamp is no longer needed** in the lock file — synthetic commit timestamps are derived from the git log of the project repo. One less field to maintain.

### CLI Commands

#### `azldev component bump <component>`

Increments the `release` counter in the lock file for the named component. Both `bump` and `update` recompute and store the fingerprint after any mutation — this prevents the next `update` from seeing a stale fingerprint and double-bumping.

```bash
$ azldev component bump curl
curl: release 3 → 4

$ azldev component bump glibc
glibc: release 5 → 6
```

For `manual` strategy components, `bump` returns an error: *"kernel uses manual release strategy; update the spec's Release: tag directly."*

The "why" behind a bump is captured by the PR description and git commit message — not in the lock file. The lock file is a computed state file; human context belongs in the VCS history.

#### `azldev component update [-a | <component>...] [--no-bump]`

Re-resolves upstream commits and auto-bumps changed components. Uses the standard component selector (`-a`/`--all-components` for all, or named components). See [Upstream Commit Pinning](#upstream-commit-pinning) for details. This is the primary workflow for moving the snapshot forward, updating upstream sources, or any change that affects multiple components at once.

#### `azldev component bump --set <n> <component>`

Allows explicitly setting the release count (for migration or correction).

#### `azldev component bump --check`

CI validation mode. Recomputes the current fingerprint for each component (using the lock file's `upstream-commit` for source identity — no network needed) and compares to the stored `fingerprint`. Exits non-zero if any component's fingerprint differs, indicating a bump is needed. Does not modify the lock file.

### Integration Points

#### 1. Fingerprint (`internal/fingerprint/`)

Replace `AffectsCommitCount int` in `ComponentInputs` and `IdentityOptions` with a `Release int` field (or rename in place). `combineInputs()` writes `release_count` instead of `affects_commit_count`. The value is read from the lock file instead of calling `FindAffectsCommits()`. For `manual` strategy components, `release_count` is `0` (no lock file entry). For `explicit` components, the `release_count` is the lock file value — the implicit `Release:` overlay is NOT part of the config hash (it's not in the config file), so the release number affects the fingerprint solely through `release_count`.

`SpecSource.UpstreamCommit` must be tagged with `fingerprint:"-"` to exclude it from `ConfigHash`. Since the lock file is now the sole source of truth for upstream commits (via `SourceIdentity`), including `UpstreamCommit` in the config hash would double-count the same information through two channels and cause false-positive fingerprint mismatches when the TOML field is empty but the lock file has a resolved commit.

Both `bump` and `update` must recompute and store the fingerprint in the lock file after any mutation. This prevents the next `update` from seeing a stale fingerprint and double-bumping.

This is a **fingerprint-breaking change** (all fingerprints will change on the switchover commit) but that is acceptable since we are doing a hard cut. The `AffectsCommitCount` → `Release` rename and the `fingerprint:"-"` tag addition should happen in the same commit to keep the break atomic.

#### 2. Source identity (`internal/providers/sourceproviders/`)

`FedoraSourcesProviderImpl.ResolveSourceIdentity()` reads the lock file's `upstream-commit`. If missing or stale vs. config, it returns an error — no silent fallback. This makes identity resolution faster, deterministic, and catches stale lock files early.

`FedoraSourcesProviderImpl.GetComponent()` uses the locked commit for checkout via `checkoutTargetCommit()`.

#### 3. Synthetic history (`internal/app/azldev/core/sources/synthistory.go`)

`buildSyntheticCommits()` currently returns `[]CommitMetadata` from scanning `Affects:` commits, and `CommitSyntheticHistory()` replays them as git commits so rpmautospec sees the right commit count.

With lock files:

- Read `release` count from the lock file.
- For upstream components: preserve the upstream dist-git `.git` history. Append `release + 1` synthetic commits on top: one baseline commit (captures overlay changes via `git add -A`) plus `release` additional empty commits.
- For local components: fresh git repo, `release + 1` synthetic commits.
- **Commit messages**: extracted from the project repo's git log — commits that changed this component's entry in `azldev.lock`. If fewer git log messages than `release`, pad with generic entries. If more, use the latest N.
- **Timestamps**: derived from the git log of the project repo (commit author dates). No `last-bumped` field needed in the lock file.
- For `explicit` strategy with `%changelog`: `rpmdev-bumpspec` is used instead of synthetic commits to add proper `%changelog` entries and set the `Release:` tag.
- This is simpler than the current approach (no git log walking for `Affects:` trailers, no metadata preservation from `Affects:` commits).

#### 4. Identity command (`internal/app/azldev/cmds/component/identity.go`)

`countAffectsCommits()` is replaced with `readReleaseCount()` which reads from the lock file. No git repository access needed — faster, deterministic, rebase-safe.

#### 5. Source preparation (`internal/app/azldev/core/sources/sourceprep.go`)

`trySyntheticHistory()` uses the new lock file–based commit generation instead of git log scanning.

For `explicit` strategy components with `%changelog`, source preparation uses `rpmdev-bumpspec` to set the `Release:` tag and add `%changelog` entries. For `explicit` with `%autochangelog`, the implicit `spec-set-tag Release:` overlay is used instead, and changelog entries come from synthetic commits (same as `auto`). If a user has a `spec-set-tag` overlay targeting the `Release:` tag AND the component uses `explicit` strategy, it is an error — don't have two sources of truth for `Release:`.

#### 6. Local component support

For local components, `update` recomputes the fingerprint from the local spec content hash + config + overlays and auto-bumps if the fingerprint changed. No upstream commit resolution is needed — there is no `upstream-commit` to re-resolve.

#### 7. CI Validation

CI can validate that lock files are consistent:

```bash
# Compute identities for base and head
azldev component identity -a -O json > head-identity.json

# Check: did any component's fingerprint change without a corresponding lock file bump?
azldev component bump --check  # exits non-zero if any component needs bumping
```

### What Gets Removed

- `FindAffectsCommits()` function and its `MessageAffectsComponent()` helper.
- `countAffectsCommits()` in `identity.go`.
- `buildSyntheticCommits()` git log scanning path (replaced with lock file read + synthetic commit generation).
- All references to `AffectsCommitCount` (renamed to `Release`).
- Tests for `Affects:` trailer parsing.

### What Stays the Same

- `CommitSyntheticHistory()` — still creates synthetic git commits, just driven by a count from a file instead of git log parsing.
- rpmautospec — no changes needed, it still counts commits.
- The overlay system — unchanged.
- `diff-identity` — unchanged, it compares fingerprint JSON files.
- All other fingerprint inputs (config hash, source identity, overlay hashes, distro) — unchanged.
- `resolveEffectiveCommitHash()` — still exists, but only used by the `update` command. No longer called during builds or identity resolution.

## Scope

### In Scope

- Lock file format and TOML schema (release count + upstream commit + fingerprint + version header).
- Single `azldev.lock` file at project root with `version = 1`.
- `release-strategy` config field on `ComponentBuildConfig` (`auto`, `explicit`, `manual`).
- `azldev component bump` CLI command (with `--set`).
- `azldev component update` CLI command (re-resolves upstream commits + auto-bumps changed components).
- Fingerprint storage in lock file, used by `update` for auto-bump and by CI for validation.
- Lock file reader integrated into fingerprint computation and identity command.
- Lock file–based upstream commit resolution in `FedoraSourcesProviderImpl`.
- Preserved upstream dist-git history for changelog generation.
- Changelog entries derived from git log of lock file + upstream history.
- `rpmdev-bumpspec` integration for `explicit` strategy (`%changelog` + `Release:` tag).
- Lock file–driven synthetic history generation.
- Removal of `Affects:` commit scanning code.
- Unit tests for all new code.
- Update to user-facing docs (config reference, how-to for bump workflow).

### Out of Scope

- Automated migration tool (manual cleanup is fine given minimal existing usage).
- Changes to rpmautospec or the synthetic history mechanism itself.

### Follow-on: Rendered Spec Output

`azldev component render -a` writes final post-overlay specs to a checked-in directory (e.g., `rendered/`). These are generated artifacts (like `docs/user/reference/cli/`), regenerated by `render` and committed alongside the lock file. This feature depends on the lock file infrastructure from this PRD but is scoped separately. **High priority** — to be started immediately after lock file support lands.

## Success Criteria

1. Release bump count is stored in a tracked, diffable file — no git log scanning required.
2. `azldev component bump curl` increments the count and produces a visible file diff.
3. Fingerprint computation is deterministic and rebase-safe (same repo state = same fingerprint, regardless of git history).
4. Upstream commit resolution is deterministic — `upstream-commit` in the lock file is used for builds, immune to backdated upstream merges.
5. `azldev component update` re-resolves upstream commits and auto-bumps changed components in one step.
6. Synthetic history generation produces the correct commit count from the lock file.
7. `%autochangelog` produces meaningful per-release entries derived from git log of the lock file, with upstream history preserved.
8. CI can detect missing bumps by comparing fingerprints to lock file state.
9. No regression in `azldev component identity`, `diff-identity`, or `component prepare-sources` workflows.
10. `release-strategy = "manual"` components are skipped by `bump --auto` and clearly identified in `identity` output.
11. `release-strategy = "explicit"` components store `release` in the lock file; the value is applied as an implicit `spec-set-tag` overlay during source preparation.
12. `release-strategy = "manual"` components reject `azldev component bump` with a clear error.

## Implementation Considerations

- Lock file loading should integrate with the existing `projectconfig` loader (`configfile.go` / `loader.go`) for path resolution and error reporting.
- Single `azldev.lock` at project root — no per-component lock files.
- The `release` field default is `0`, not `1` — a component with no lock file entry has never been bumped.
- Lock file entries for components that don't exist in the config should produce a warning (stale entry after component removal).
- The lock file should be sorted alphabetically by component name for deterministic diffs.
- Overlay descriptions should be encouraged via linting/warnings when a description is empty, but not enforced as a hard requirement.
- `update` should warn (but not error) if a component's resolved commit didn't change — this lets automation run `update` idempotently.
- `update --no-bump` records the new upstream state without bumping — useful for inspecting changes before committing.
- `release-strategy` defaults to `auto`. Existing components without it set behave exactly as today.
- For `explicit` components with `%changelog`, source preparation uses `rpmdev-bumpspec` to set the `Release:` tag and add changelog entries. For `explicit` with `%autochangelog`, an implicit `spec-set-tag` overlay is used for `Release:` and changelog comes from synthetic commits.
- For `manual` components, `azldev component bump` returns an error directing the user to update the spec directly.
- `release` is a monotonically increasing counter starting at 0. It never resets. For `auto` strategy, it means "number of additional bumps on top of upstream" — total `%autorelease` = upstream commits + release + 1. For `explicit` strategy, it is the value passed to `rpmdev-bumpspec` or used as the `Release:` tag.
- First bumps: `bump --set N` can bootstrap the initial value if needed.
- `rpmdev-bumpspec` must be available in the build environment for `explicit` strategy components that use `%changelog`. This is already the case in mock environments.
- Concurrent lock file writes are not a concern for single-threaded CLI usage. In parallel CI, different TOML sections merge cleanly via git.
- The lock file must include `version = 1` for forward compatibility. Future azldev versions can detect and migrate old formats.
- `upstream-commit` in the lock file is always a full 40-character hex hash. TOML config accepts short hashes (7–40 chars); `update` resolves to full length.
- If a user has a `spec-set-tag` overlay targeting the `Release:` tag AND the component uses `explicit` strategy, it is a config error — two sources of truth for `Release:` are not allowed.

## Dependencies

- This work builds on the component change detection system (bn-c5ae), specifically the fingerprint and `diff-identity` infrastructure.
- The auto-bump behavior of `update` depends on fingerprint storage in the lock file, which is now a required feature.

## Future Extensions

The rendered spec output (see [Follow-on: Rendered Spec Output](#follow-on-rendered-spec-output)) is the primary planned extension. Additional future possibilities include:

## Resolved Questions

1. **Lock file always committed** — it is the source of truth for release state. Not `.gitignore`-able.
2. **`bump` modifies the file only** — does not auto-commit. Developers commit with their PR; they can script auto-commit if desired.
3. **Inline components supported** — the project-level `azldev.lock` handles all components.
4. **Lock file is the single source of truth** — no priority chain. Missing `upstream-commit` for upstream components is an error; missing `release` defaults to `0`. `spec.upstream-commit` in TOML is an input to `update`, not a runtime override. Matches the `go.mod`/`go.sum` model.
5. **Single lock file** — one `azldev.lock` at the project root. Tools write it; humans don't.
