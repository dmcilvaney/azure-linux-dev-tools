# Changelog

<!-- markdownlint-disable-file MD013 MD024 -->

All notable changes to `azldev` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0] - 2026-07-01

### Added

- **Component lifecycle and reproducibility**
  - Added lock file foundations and expanded them to include local components.
  - Added deterministic component fingerprints, lock validation/orphan detection, and structural validation checks.
  - Added `azldev component changed` and persisted input fingerprints during `component update`.
  - Added freshness and upstream-commit staleness checks to optimize update behavior.
- **Spec rendering, overlays, and source preparation**
  - Added `azldev component render` plus check-only mode (`--check-only`) for render and update.
  - Added new overlay capabilities: `spec-remove-section`, `spec-remove-subpackage`, `source-files` replacement, inline `[metadata]`, and per-file `overlay-files`.
  - Added options for source preparation, including skipping lookaside/source tar downloads.
  - Added deterministic archive extract/repack utilities.
- **Build and release controls**
  - Added explicit release calculation modes (`autorelease`, `static`) and improved static release handling.
  - Added manual rebuild control via `--bump`.
  - Added support for dirty commit metadata when config has uncommitted changes during dist-git generation.
- **CLI surface expansion**
  - Added `azldev package list` (with `--rpm-file` and `--synthesize-debug-packages`).
  - Added `azldev repo query` plus RPM repo resources and repo-set templates.
  - Added `download-sources` and richer `component list` output (rendered spec location, split package/component group columns).
  - Added user-level config loading from XDG config home.
- **Image and test tooling**
  - Added image capabilities (including booting livecd-style ISOs), test suites, and publish config.
  - Added pytest suite support.
  - Added gremlins mutation testing integration via mage and MCP.
- **Developer experience**
  - Added progress bar output and actionable command/help hints.
  - Added explicit docs generation mage target.
  - Added an environment-variable escape hatch for root-user checks.

### Fixed

- Make patch discovery work across whole spec (#18)
- Replace raw `%s` with `%#q` (#56)
- Skip .git directory when applying file-targeting overlays (#64)
- Remove exact azldev version from spec headers (#66)
- Improve error when no image specified (#70)
- Make `sources-files` update `sources` (#69)
- Allow download-sources to run as root (#89)
- Remove truncated (e.g.) example from hash-type schema description (#87)
- Suppress wait messages and progress bars in quiet mode (#41)
- Handle additional %autorelease macro forms in Release tag detection (#91)
- Bump testcontainers-go to v0.42.0 and fix moby/moby import breaking change (#115)
- Parse URL sources that use the rename '#' tag (#118)
- Improve shallow clone guidance for synthetic history (#105)
- Support git worktrees in synthetic history generation (#125)
- URL-escape placeholder values in BuildLookasideURL and dist-git URL construction (#74)
- Swap boot order so disk has priority over ISO (#135)
- Disallow release bumping on decimal releases (#138)
- Fix render staging, import-commit seeding, and commit parsing (#139)
- Scrub submodules from distgits (#137)
- Restore git worktree support in synthetic history generation (#145)
- Consume merge-commits in dist-git (#146)
- Count unnumbered Patch: tags in GetHighestPatchTagNumber (#160)
- Don't return null json when getting empty slices (#167)
- Error on source vs. identity drift (#169)
- Allow autobump for Non Conditional %{dist} Releases (#179)
- Handle -l flag in spec section header package name extraction (#180)
- Handle -- trigger terminator and -P flag in spec section header parsing (#189)
- Use first-parent for snapshot-time commit resolution (#192)
- Balance conditional nesting when removing spec sections (#190)
- Make AddFixSuggestion thread-safe (#183)
- Disable validation checks when passing --permissive-config (#216)
- Filter noise from license checks (#230)
- Surface close errors and drop unused writable handle in completions (#233)
- Expand overlay files after config resolution

## [0.1.0] - 2026-03-18

First tagged preview release of `azldev`, the developer CLI for the
[Azure Linux](https://github.com/microsoft/azurelinux) distro.

### Added

- **Project and metadata management.** Scaffold a project with `azldev project
  init` or `project new`, then parse, resolve, and query the TOML metadata
  (`azldev.toml`) that defines Azure Linux. Configuration merges built-in
  defaults with project and user-level (XDG) files, is fully validated, and is
  published as a JSON Schema via `azldev config generate-schema`.
- **Component inspection and locking.** List and inspect components with `azldev
  component list` and `component query`, and import new ones with `component
  add`. Deterministic component fingerprints and per-component lock files keep
  builds reproducible; `component update` refreshes them with `--check-only`,
  `--bump`, freshness-based skipping, a progress bar, and upstream-staleness
  detection. `component changed` and `component diff-sources` report what moved.
- **Source preparation and spec rendering.** `component prepare-sources` and
  `component render` produce build-ready sources and specs through a
  `mock`-based batch pipeline, synthesizing the git history that `rpmautospec`
  needs and constructing dist-git from lock-file history. A rich overlay system
  (spec search/replace, prepend/append lines, remove section or subpackage, file
  and source replacement, per-file overlay files, and inline metadata)
  customizes specs, with explicit release-calculation modes (`autorelease`,
  `static`, and automatic). Source archives are fetched from lookaside caches.
- **Local package and image builds.** Build individual packages with `mock`
  using `component build`, emitting RPMs and SRPMs into structured,
  publish-channel-aware output directories. `azldev image` builds, customizes,
  injects files into, boots, and runs LISA tests against Azure Linux images on a
  local QEMU VM.
- **Package and repository queries.** Inspect binary package configuration with
  `azldev package list` (including `--rpm-file`, debug-package synthesis, and
  separate package/component group columns), and inspect or manage RPM
  repositories with `azldev repo query`, backed by repo resources and repo-set
  templates.
- **Command-line experience.** Shell completions for bash, zsh, fish, and
  PowerShell; actionable hints on errors; global `--quiet`, `--verbose`, and
  `--dry-run` flags with `table`, `json`, `csv`, and `markdown` output formats;
  an embedded MCP server (`azldev advanced mcp`); and auto-generated CLI
  reference documentation.
