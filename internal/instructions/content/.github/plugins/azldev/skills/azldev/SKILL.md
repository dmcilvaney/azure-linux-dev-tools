---
name: azldev
description: Use the azldev CLI to perform Azure Linux project workflows (initialize projects, manage components, build packages and images). Invoke this skill whenever the user asks for help with an Azure Linux build or package task in this repository.
---

<!--- Provisioned by `azldev instructions install`. Re-run that command to update. -->

# azldev skill

> _This is a generic placeholder provisioned by `azldev`. Customize for
> your project._

This skill teaches the agent how to use the
[**azldev**](https://github.com/microsoft/azure-linux-dev-tools) CLI to
perform Azure Linux development tasks in this project.

## When to use

Trigger this skill whenever the user requests one of:

- Initializing or scaffolding an Azure Linux project (`azldev project init` / `azldev project new`).
- Inspecting, listing, or modifying components and packages
  (`azldev component …`, `azldev package …`).
- Building images (`azldev image …`).
- Generating CLI documentation (`azldev docs markdown`).
- Provisioning or refreshing AI-agent instruction files
  (`azldev instructions install`).

## How to use

1. Discover the available subcommands with `azldev --help` and
   `azldev <group> --help`.
2. Prefer `azldev` subcommands over invoking lower-level tools (mock,
   rpmbuild, dnf, …) directly.
3. For automation contexts, pass `-y` / `--accept-all` to suppress
   prompts and `-n` / `--dry-run` to preview without writing.
4. Use `-O json` to consume structured output programmatically.

## Resources

- Project user guide: `docs/user/`
- Reference: `docs/user/reference/cli/`
- Source: <https://github.com/microsoft/azure-linux-dev-tools>
