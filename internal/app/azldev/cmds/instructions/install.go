// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package instructions

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	libinstructions "github.com/microsoft/azure-linux-dev-tools/internal/instructions"
	"github.com/spf13/cobra"
)

// InstallOptions holds the flag-controlled options for `azldev instructions install`.
type InstallOptions struct {
	// User selects the per-user destination instead of the project.
	User bool
	// Force overwrites existing files instead of skipping them.
	Force bool
}

// InstallResult is the structured result returned by the command for
// machine-readable output formats.
type InstallResult struct {
	Scope    string   `json:"scope"`
	DestBase string   `json:"destBase"`
	Written  []string `json:"written"`
	Skipped  []string `json:"skipped"`
}

func installOnAppInit(_ *azldev.App, parentCmd *cobra.Command) {
	parentCmd.AddCommand(NewInstallCmd())
}

// NewInstallCmd constructs the `azldev instructions install` command.
func NewInstallCmd() *cobra.Command {
	options := &InstallOptions{}

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install (or update) AI-agent instruction files",
		Long: `Install or update a starter set of AI-agent instruction files.

By default writes into the current project (the current working
directory). Pass --user to install into the per-user configuration
directory instead, which is useful for global guidance that should
apply across every project on the host.

Existing files are skipped unless --force is provided. Use the global
-y / --accept-all flag to suppress the interactive overwrite
confirmation.

The set of files installed currently includes:

  - AGENTS.md                              (open agent convention)
  - .github/copilot-instructions.md        (GitHub Copilot)
  - .github/instructions/azldev.instructions.md  (VS Code Copilot Chat)
`,
		Example: `  # Install into the current project
  cd my-project && azldev instructions install

  # Update files in-place (overwrites local edits!)
  azldev instructions install --force

  # Install into the user's global config directory
  azldev instructions install --user

  # Preview without writing anything
  azldev instructions install --dry-run`,
		RunE: azldev.RunFuncWithoutRequiredConfig(func(env *azldev.Env) (interface{}, error) {
			return runInstall(env, options)
		}),
	}

	cmd.Flags().BoolVar(&options.User, "user", false,
		"install into the user's per-user config dir instead of the project")
	cmd.Flags().BoolVarP(&options.Force, "force", "f", false,
		"overwrite existing instruction files instead of skipping them")

	return cmd
}

// runInstall is the command body, factored out for testability.
func runInstall(env *azldev.Env, options *InstallOptions) (*InstallResult, error) {
	scope, destBase, err := resolveDestination(env, options)
	if err != nil {
		return nil, err
	}

	// Confirm overwriting if --force is set and prompts are allowed.
	if options.Force && !env.AllPromptsAccepted() {
		if env.PromptsAllowed() {
			ok := env.ConfirmAutoResolution(fmt.Sprintf(
				"Overwrite any existing instruction files under %#q?", destBase))
			if !ok {
				return nil, errors.New("instructions install aborted by user")
			}
		} else {
			// Non-interactive shell without -y: refuse rather than silently overwrite.
			return nil, errors.New(
				"refusing to --force overwrite without confirmation; pass -y / --accept-all to proceed")
		}
	}

	libResult, err := libinstructions.Provision(env, env.FS(), scope, destBase, libinstructions.Options{
		Force:     options.Force,
		AssumeYes: env.AllPromptsAccepted(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to install instruction files:\n%w", err)
	}

	if len(libResult.Skipped) > 0 && !options.Force {
		slog.Info(
			"Some instruction files already exist and were left in place; pass --force to overwrite",
			"skippedCount", len(libResult.Skipped))
	}

	slog.Info("Installed instruction files",
		"scope", scope.String(),
		"destBase", libResult.DestBase,
		"writtenCount", len(libResult.Written),
		"skippedCount", len(libResult.Skipped))

	return &InstallResult{
		Scope:    scope.String(),
		DestBase: libResult.DestBase,
		Written:  libResult.Written,
		Skipped:  libResult.Skipped,
	}, nil
}

// resolveDestination picks the install scope and destination base path
// based on the user-supplied options.
func resolveDestination(
	env *azldev.Env, options *InstallOptions,
) (libinstructions.Scope, string, error) {
	if options.User {
		destBase, err := libinstructions.UserDestBase(env.OSEnv())
		if err != nil {
			return libinstructions.ScopeUser, "",
				fmt.Errorf("failed to determine user destination:\n%w", err)
		}

		return libinstructions.ScopeUser, destBase, nil
	}

	cwd, err := env.OSEnv().Getwd()
	if err != nil {
		return libinstructions.ScopeProject, "",
			fmt.Errorf("failed to get current working directory:\n%w", err)
	}

	abs, err := filepath.Abs(cwd)
	if err != nil {
		return libinstructions.ScopeProject, "",
			fmt.Errorf("failed to resolve absolute path for %#q:\n%w", cwd, err)
	}

	return libinstructions.ScopeProject, abs, nil
}
