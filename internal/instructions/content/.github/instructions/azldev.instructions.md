---
applyTo: "**/*"
description: "Project-wide guidance for using azldev. Provisioned by `azldev instructions install`."
---

<!--- Provisioned by `azldev instructions install`. Re-run that command to update. -->

# azldev Instructions

> _This is a generic placeholder provisioned by `azldev`. Customize
> for your project._

This file is auto-loaded by VS Code Copilot Chat for files matching
the `applyTo` pattern in the front-matter above. Add additional
`*.instructions.md` files in this directory for language- or
path-specific guidance (set a more specific `applyTo` glob in each).

## Working with azldev

This project is built and managed with
[**azldev**](https://github.com/microsoft/azure-linux-dev-tools).
When suggesting commands or edits:

- Prefer `azldev` subcommands over raw tool invocations.
- Discover available commands with `azldev --help` and
  `azldev <group> --help`.
- The CLI supports a global `-y` / `--accept-all` flag for
  non-interactive automation, and `-n` / `--dry-run` to preview
  changes without writing them.
- See `docs/user/` (if present) for the full user guide.

## Keeping this file up to date

Re-run `azldev instructions install` to refresh this file. To stop
managing it via azldev, simply delete this file or edit it freely —
azldev will not overwrite it without `--force`.
