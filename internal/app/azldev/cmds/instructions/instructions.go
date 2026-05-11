// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package instructions registers the `azldev instructions` CLI command
// group, which provisions AI-agent instruction files (Copilot, Copilot
// CLI, AGENTS.md, ...) into a project repo or a user's global config.
package instructions

import (
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/spf13/cobra"
)

// OnAppInit registers the `instructions` command group with the app.
func OnAppInit(app *azldev.App) {
	cmd := &cobra.Command{
		Use:   "instructions",
		Short: "Manage AI-agent instruction files (Copilot, AGENTS.md, ...)",
		Long: `Manage AI-agent instruction files for the project.

Provisions a starter set of instruction files (using widely-adopted
conventions like AGENTS.md, .github/copilot-instructions.md, and
VS Code Copilot Chat .instructions.md files) into the current project
or into the user's global configuration directory. The same command
can be re-run later to keep those files up to date.`,
	}

	app.AddTopLevelCommand(cmd)
	installOnAppInit(app, cmd)
}
