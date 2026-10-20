# How To: Build an Image

This guide walks through building an Azure Linux image with `azldev`.

## Prerequisites

- A configured Azure Linux project with at least one image definition (see [Creating a Project](create-project.md))
- Built RPMs available in a local repository or remote package source
- The `azldev` CLI installed and on your `PATH`

Image builds currently reject KIWI 11.0.0 and newer, including KIWI 11 prereleases.
KIWI 11 changed its runtime configuration format and is not supported by this
azldev release. azldev checks `kiwi --version` before building and reports an
actionable error for incompatible versions; it does not automatically downgrade
or upgrade installed packages. Use a compatible KIWI 10.x package from your
distribution or a build environment that provides it.

Dependency version checks are opt-in in azldev's code and run only when the
corresponding prerequisite is needed. Unrelated commands do not probe KIWI, and
dry runs do not execute version probes. Requirements are owned by the azldev
release, not by project TOML.

## List Available Images

```bash
azldev image list
```

## Build an Image

```bash
azldev image build
```

Follow the interactive prompts to select an image definition and target architecture.

## Common Options

| Flag | Description |
|------|-------------|
| `--arch` | Target architecture (`x86_64` or `aarch64`) |
| `--local-repo <path>` | Add a local RPM repository as a package source |
| `--remote-repo <url>` | Add a remote RPM repository as a package source |
| `--remote-repo-no-gpgcheck` | Disable GPG checking for remote repositories |
## Override KIWI Configuration

Set `kiwi-config-override` on the build distro version to override KIWI settings.
The path is resolved relative to the TOML file that declares the distro version:

```toml
[distros.azurelinux.versions.'4.0-stage1']
kiwi-config-override = "kiwi/azl4/stage1/azurelinux-4.0-kiwi-override.yml"
```

KIWI merges `--config` after its standard configuration files, giving these overrides
the highest precedence. Merging is top-level: defining a section replaces that entire
section from earlier files.

## Boot an Image

After building, boot the image in a QEMU VM for testing:

```bash
azldev image boot -i <path-to-image> -f <format> --test-password <password>
```

Supported formats: `raw`, `qcow2`, `vhdx`.

## Customize a Pre-Built Image

Use Image Customizer to modify an existing image:

```bash
azldev image customize --image-file <path> --image-config <config> --output-path <output>
```

## Related Resources

- [Images Reference](../reference/config/images.md) — image definition format
- [Tools Reference](../reference/config/tools.md) — external tool configuration (Image Customizer)

<!-- TODO: expand with kiwi image configuration examples and customization workflows -->
