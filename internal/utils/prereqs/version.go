// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package prereqs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
)

const versionProbeTimeout = 10 * time.Second

// VersionRequirement opts an external executable into version validation.
type VersionRequirement struct {
	// Constraint uses go-version comparisons, for example ">= 2.0, < 3.0".
	Constraint string
	// Args defaults to ["--version"].
	Args []string
	// Pattern optionally extracts the version using exactly one capture group.
	Pattern string
	// Guidance explains how to resolve an incompatible version.
	Guidance string
}

// Match a dotted version, retaining prerelease and build suffixes for comparison.
//

var defaultVersionPattern = regexp.MustCompile(
	`\bv?[0-9]+\.[0-9]+(?:\.[0-9]+)*(?:-?[A-Za-z][A-Za-z0-9.-]*|-[0-9][A-Za-z0-9.-]*)?(?:\+[A-Za-z0-9.-]+)?\b`,
)

// RequireVersion checks an installed executable without installing or changing it.
func RequireVersion(ctx opctx.Ctx, programName string, requirement VersionRequirement) error {
	if ctx.DryRun() {
		return nil
	}

	if !ctx.CommandInSearchPath(programName) {
		return fmt.Errorf("program %#q required:\n%w", programName, ErrMissingExecutable)
	}

	// Validate code-owned requirements before executing the probe.
	if _, _, err := requirement.parse(); err != nil {
		return fmt.Errorf("invalid version requirement for %#q:\n%w", programName, err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()

	args := requirement.Args
	if len(args) == 0 {
		args = []string{"--version"}
	}

	raw := exec.CommandContext(probeCtx, programName, args...)
	raw.WaitDelay = time.Second

	var stderr bytes.Buffer

	raw.Stderr = &stderr

	cmd, err := ctx.Command(raw)
	if err != nil {
		return fmt.Errorf("failed to prepare version probe for %#q:\n%w", programName, err)
	}

	output, err := cmd.RunAndGetOutput(probeCtx)
	if err != nil {
		return fmt.Errorf("failed to query version of %#q (stderr: %s):\n%w", programName, stderr.String(), err)
	}

	if err := requirement.CheckOutput(output + "\n" + stderr.String()); err != nil {
		return fmt.Errorf("dependency %#q version check failed:\n%w", programName, err)
	}

	return nil
}

func (r VersionRequirement) parse() (version.Constraints, *regexp.Regexp, error) {
	if r.Constraint == "" {
		return nil, nil, errors.New("version constraint must not be empty")
	}

	constraint, err := version.NewConstraint(r.Constraint)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid version constraint %#q:\n%w", r.Constraint, err)
	}

	pattern := defaultVersionPattern
	if r.Pattern != "" {
		pattern, err = regexp.Compile(r.Pattern)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid version pattern:\n%w", err)
		}

		if pattern.NumSubexp() != 1 {
			return nil, nil, errors.New("version pattern must contain exactly one capture group")
		}
	}

	return constraint, pattern, nil
}

// CheckOutput extracts a version from probe output and enforces the constraint.
func (r VersionRequirement) CheckOutput(output string) error {
	constraint, pattern, err := r.parse()
	if err != nil {
		return err
	}

	matches := pattern.FindStringSubmatch(output)
	if len(matches) == 0 {
		return fmt.Errorf("could not extract version from output %#q", output)
	}

	detected := matches[len(matches)-1]

	current, err := version.NewVersion(detected)
	if err != nil {
		return fmt.Errorf("could not parse detected version %#q:\n%w", detected, err)
	}

	if !constraint.Check(current) {
		guidance := r.Guidance
		if guidance == "" {
			guidance = "install a supported version of this dependency"
		}

		return fmt.Errorf("installed version %s does not satisfy %#q; %s",
			current.Original(), r.Constraint, guidance)
	}

	return nil
}
