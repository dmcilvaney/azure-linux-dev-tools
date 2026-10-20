// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package prereqs_test

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/testctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/prereqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionRequirement(t *testing.T) {
	for name, testCase := range map[string]struct {
		constraint string
		output     string
		pattern    string
		wantErr    string
	}{
		"equal":           {constraint: ">= 11.1.3", output: "KIWI (next generation) version 11.1.3"},
		"newer":           {constraint: ">= 2.9", output: "git version 2.10.0"},
		"older":           {constraint: ">= 11.1.3", output: "KIWI version 11.1.2", wantErr: "install a supported version"},
		"prerelease":      {constraint: ">= 11.1.3", output: "KIWI version 11.1.3-rc1", wantErr: "does not satisfy"},
		"python release":  {constraint: ">= 3.14.0", output: "Python 3.14.0rc1", wantErr: "does not satisfy"},
		"prefix":          {constraint: ">= 1.2.3", output: "tool v1.2.3"},
		"four segments":   {constraint: ">= 1.2.3.4", output: "tool 1.2.3.5"},
		"missing output":  {constraint: ">= 1.0", wantErr: "extract"},
		"unknown output":  {constraint: ">= 1.0", output: "unreleased", wantErr: "extract"},
		"custom pattern":  {constraint: ">= 11.1.3", output: "Python 3.14.0; KIWI 11.1.3", pattern: `KIWI (\S+)`},
		"custom mismatch": {constraint: ">= 11.1.3", output: "Python 3.14.0", pattern: `KIWI (\S+)`, wantErr: "extract"},
		"invalid output": {
			constraint: ">= 1.0", output: "tool release unknown", pattern: `release (\S+)`, wantErr: "parse",
		},
		"range":            {constraint: ">= 10.0, < 11.0.0-0", output: "KIWI 10.3.11"},
		"upper bound":      {constraint: "< 11.0.0-0", output: "KIWI 11.0.0", wantErr: "does not satisfy"},
		"upper prerelease": {constraint: "< 11.0.0-0", output: "KIWI 11.0.0rc1", wantErr: "does not satisfy"},
		"future major":     {constraint: "< 11.0.0-0", output: "KIWI 12.0.0", wantErr: "does not satisfy"},
		"exact":            {constraint: "= 10.3.11", output: "KIWI 10.3.11"},
		"exact mismatch":   {constraint: "= 10.3.11", output: "KIWI 10.3.10", wantErr: "does not satisfy"},
	} {
		t.Run(name, func(t *testing.T) {
			requirement := prereqs.VersionRequirement{
				Constraint: testCase.constraint,
				Pattern:    testCase.pattern,
			}

			err := requirement.CheckOutput(testCase.output)
			if testCase.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, testCase.wantErr)
			}
		})
	}
}

func TestVersionRequirementValidation(t *testing.T) {
	for name, requirement := range map[string]prereqs.VersionRequirement{
		"missing constraint": {},
		"invalid constraint": {Constraint: "latest"},
		"invalid regex":      {Constraint: ">= 1.0", Pattern: "["},
		"no capture":         {Constraint: ">= 1.0", Pattern: `\d+\.\d+`},
		"two captures":       {Constraint: ">= 1.0", Pattern: `(\d+)\.(\d+)`},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, requirement.CheckOutput("tool 1.0"))
		})
	}
}

func TestRequireVersion(t *testing.T) {
	for name, testCase := range map[string]struct {
		output   string
		stderr   string
		probeErr error
		dryRun   bool
		missing  bool
		wantErr  string
	}{
		"stdout":        {output: "tool 2.5"},
		"stderr":        {stderr: "tool 2.5"},
		"wrong version": {output: "tool 3.0", wantErr: "use the supported tool 2.x package"},
		"probe failed": {
			stderr: "broken installation", probeErr: errors.New("exit status 1"), wantErr: "broken installation",
		},
		"unknown output": {output: "unknown", wantErr: "extract"},
		"dry run":        {dryRun: true},
		"missing":        {missing: true, wantErr: "required"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := testctx.NewCtx()

			ctx.DryRunValue = testCase.dryRun
			if !testCase.missing {
				ctx.CmdFactory.RegisterCommandInSearchPath("tool")
			}

			probes := 0
			ctx.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
				probes++

				assert.Equal(t, []string{"tool", "--version"}, cmd.Args)
				_, err := fmt.Fprint(cmd.Stderr, testCase.stderr)
				require.NoError(t, err)

				return testCase.output, testCase.probeErr
			}

			err := prereqs.RequireVersion(ctx, "tool", prereqs.VersionRequirement{
				Constraint: ">= 2.0, < 3.0",
				Guidance:   "use the supported tool 2.x package",
			})
			if testCase.wantErr != "" {
				require.ErrorContains(t, err, testCase.wantErr)
				require.ErrorContains(t, err, "tool")
			} else {
				require.NoError(t, err)
			}

			if testCase.dryRun || testCase.missing {
				assert.Zero(t, probes)
			} else {
				assert.Equal(t, 1, probes)
			}
		})
	}
}

func TestRequireExecutableVersionOptIn(t *testing.T) {
	ctx := testctx.NewCtx()
	ctx.CmdFactory.RegisterCommandInSearchPath("tool")

	probes := 0
	ctx.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		probes++

		assert.Equal(t, []string{"tool", "version", "--short"}, cmd.Args)

		return "Python 3.14.0; release=2.5", nil
	}
	require.NoError(t, prereqs.RequireExecutable(ctx, "tool", nil))
	require.NoError(t, prereqs.RequireExecutable(ctx, "tool", &prereqs.PackagePrereq{}))
	assert.Zero(t, probes)
	require.NoError(t, prereqs.RequireExecutable(ctx, "tool", &prereqs.PackagePrereq{
		Version: &prereqs.VersionRequirement{
			Constraint: ">= 2.0", Args: []string{"version", "--short"}, Pattern: `release=(\S+)`,
		},
	}))
	assert.Equal(t, 1, probes)
}
