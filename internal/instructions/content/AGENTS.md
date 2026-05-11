<!--- Provisioned by `azldev instructions install`. Re-run that command to update. -->

# AGENTS.md

This file provides high-level guidance for AI coding agents (such as
GitHub Copilot, OpenAI Codex, Cursor, and Claude Code) operating on
this repository. It uses the open
[`AGENTS.md`](https://agents.md/) convention so that a single
file is recognized by a wide range of tools.

## About this project

> _This is a generic placeholder provisioned by `azldev`. Customize
> the sections below for your project._

This repository uses
[**azldev**](https://github.com/microsoft/azure-linux-dev-tools) — the
Azure Linux Dev Tool — to manage components, build packages, and
produce images.

## Build & test commands

Use `azldev` for project-level operations and add any project-specific
commands here, for example:

- `azldev project init` — initialize a new Azure Linux project
- `azldev component <name> ...` — operate on a single component
- `azldev image ...` — build images
- `azldev docs markdown -o docs/` — regenerate CLI reference docs

Run `azldev --help` to discover available commands.

## Conventions

> _Document repository-specific conventions here (commit message
> format, code style, review process, etc.). When unsure, prefer the
> existing conventions in the surrounding code._

## See also

- `.github/copilot-instructions.md` — entry point for GitHub Copilot
- `.github/instructions/*.instructions.md` — scoped (per-language /
  per-path) instructions for VS Code Copilot Chat
