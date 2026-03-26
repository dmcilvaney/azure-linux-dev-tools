// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rpm

import (
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/buildenv"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
)

// SpecQuerier is a wrapper around querying RPM spec files using the `rpmspec` command. The latter is
// executed within an isolated environment, ensuring insulation from the host system.
type SpecQuerier struct {
	buildEnv     buildenv.BuildEnv
	buildOptions BuildOptions
}

// SpecInfo encapsulates information extracted from an RPM spec file.
type SpecInfo struct {
	Name          string
	Version       Version
	NEVR          string
	Subpackages   []PackageNEVR
	RequiredFiles []string
}

// NewSpecQuerier constructs a new [SpecQuerier] instance that will use the provided [buildenv.BuildEnv]
// to run commands in an isolated environment. The provided [BuildOptions] will be used to
// influence the environment within which the spec is queried.
func NewSpecQuerier(buildEnv buildenv.BuildEnv, buildOptions BuildOptions) *SpecQuerier {
	return &SpecQuerier{
		buildEnv:     buildEnv,
		buildOptions: buildOptions,
	}
}

// QuerySpec queries the given spec file, returning information it parsed from the spec.
func (q *SpecQuerier) QuerySpec(ctx opctx.Ctx, specPath string) (specInfo *SpecInfo, err error) {
	const specDirPathInBuildEnv = "/spec"

	// Bind-mount the spec's dir into a known dir.
	buildEnvOpts := &buildenv.RunOptions{}
	buildEnvOpts.BindMounts = append(buildEnvOpts.BindMounts, buildenv.BindMount{
		PathInHost:     filepath.Dir(specPath),
		PathInBuildEnv: specDirPathInBuildEnv,
	})

	// Compose the rpmspec command line; make sure we use paths that will resolve within the mock root.
	specPathInMockRoot := path.Join(specDirPathInBuildEnv, filepath.Base(specPath))
	rpmspecCmdline := q.composeRpmspecCmdline(specPathInMockRoot)

	// Run rpmspec and capture its output.
	output, err := runInBuildEnvAndGetOutput(ctx, q.buildEnv, buildEnvOpts, rpmspecCmdline)
	if err != nil {
		// Look through stdout for obvious errors to report.
		for _, stdoutLine := range strings.Split(output, "\n") {
			stdoutLine = strings.TrimSpace(stdoutLine)

			if strings.HasPrefix(stdoutLine, "error:") || strings.HasPrefix(stdoutLine, "warning:") {
				slog.Error("error parsing spec", "error", stdoutLine, "specPath", specPath)
			}
		}

		return nil, fmt.Errorf("failed to run rpmspec in isolated root to parse spec '%s':\n%w", specPath, err)
	}

	// Parse the output from rpmspec. Note that we'll need to be careful to ignore warnings and errors
	// intermixed with intentional output.
	return parseRpmspecOutput(specPath, output)
}

// subpackageSeparator is the delimiter used between subpackage entries in rpmspec binary query output.
const subpackageSeparator = "---"

// QuerySubpackages queries the given spec file for all binary subpackages it produces,
// returning the [PackageNEVR] for each subpackage. This runs rpmspec without --srpm to enumerate
// all binary packages defined in the spec.
func (q *SpecQuerier) QuerySubpackages(ctx opctx.Ctx, specPath string) ([]PackageNEVR, error) {
	const specDirPathInBuildEnv = "/spec"

	// Bind-mount the spec's dir into a known dir.
	buildEnvOpts := &buildenv.RunOptions{}
	buildEnvOpts.BindMounts = append(buildEnvOpts.BindMounts, buildenv.BindMount{
		PathInHost:     filepath.Dir(specPath),
		PathInBuildEnv: specDirPathInBuildEnv,
	})

	// Compose the rpmspec command line for binary packages (no --srpm).
	specPathInMockRoot := path.Join(specDirPathInBuildEnv, filepath.Base(specPath))
	rpmspecCmdline := q.composeBinaryRpmspecCmdline(specPathInMockRoot)

	// Run rpmspec and capture its output.
	output, err := runInBuildEnvAndGetOutput(ctx, q.buildEnv, buildEnvOpts, rpmspecCmdline)
	if err != nil {
		for _, stdoutLine := range strings.Split(output, "\n") {
			stdoutLine = strings.TrimSpace(stdoutLine)

			if strings.HasPrefix(stdoutLine, "error:") || strings.HasPrefix(stdoutLine, "warning:") {
				slog.Error("error parsing spec for subpackages", "error", stdoutLine, "specPath", specPath)
			}
		}

		return nil, fmt.Errorf("failed to run rpmspec in isolated root to query subpackages for spec %#q:\n%w",
			specPath, err)
	}

	return parseBinaryRpmspecOutput(specPath, output)
}

func runInBuildEnvAndGetOutput(
	ctx opctx.Ctx, buildEnv buildenv.BuildEnv, buildEnvOpts *buildenv.RunOptions, args []string,
) (output string, err error) {
	cmd, err := buildEnv.CreateCmd(ctx, args, *buildEnvOpts)
	if err != nil {
		return output, fmt.Errorf("failed to create command to run in isolated environment:\n%w", err)
	}

	cmd.SetLongRunning("Waiting for command...")

	output, err = cmd.RunAndGetOutput(ctx)
	if err != nil {
		slog.Debug("output from failed command in isolated environment", "stdout", output)

		return output, fmt.Errorf("failed to run command in isolated environment:\n%w", err)
	}

	return strings.TrimSpace(output), nil
}

func (q *SpecQuerier) composeRpmspecCmdline(specPath string) (result []string) {
	specDirPath := filepath.Dir(specPath)

	// Compose command. Set up some fixed defines. Later we'll add the user-defined ones.
	result = []string{
		"rpmspec",
		"-q",
		"--srpm",
		"-D", "_sourcedir " + specDirPath,
		"-D", "_specdir " + specDirPath,
		"-D", "with_check 0",
		"--queryformat",
		"name=%{name}\nepoch=%{epoch}\nversion=%{version}\nrelease=%{release}\n[source=%{SOURCE}\n][patch=%{PATCH}\n]",
	}

	for _, name := range q.buildOptions.With {
		result = append(result, "--with", name)
	}

	for _, name := range q.buildOptions.Without {
		result = append(result, "--without", name)
	}

	for key, value := range q.buildOptions.Defines {
		result = append(result, "-D", fmt.Sprintf("%s %s", key, value))
	}

	result = append(result, specPath)

	return result
}

//nolint:cyclop // This function's complexity is due to the if/else-if cases for parsing.
func parseRpmspecOutput(specPath, output string) (specInfo *SpecInfo, err error) {
	var name, epoch, version, release string

	requiredFiles := []string{}

	// Go through each of the lines, trying to extract what we were looking for.
	for _, line := range strings.Split(output, "\n") {
		// Ignore whitespace-only lines.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Ignore non-fatal errors from rpmspec (e.g., complaints about changelog entries).
		//nolint:nestif // We don't really have a better way of expressing this.
		if strings.HasPrefix(trimmed, "error: ") || strings.HasPrefix(trimmed, "warning: ") {
			slog.Debug("Ignoring rpmspec error", "line", trimmed)
		} else if after, ok := strings.CutPrefix(trimmed, "name="); ok {
			name = after
		} else if after, ok := strings.CutPrefix(trimmed, "epoch="); ok {
			// Handle the case where epoch is not set, which rpmspec reports as "(none)".
			if after == "(none)" {
				epoch = "0"
			} else {
				epoch = after
			}
		} else if after, ok := strings.CutPrefix(trimmed, "version="); ok {
			version = after
		} else if after, ok := strings.CutPrefix(trimmed, "release="); ok {
			release = after
		} else if after, ok := strings.CutPrefix(trimmed, "source="); ok {
			requiredFiles = append(requiredFiles, after)
		} else if after, ok := strings.CutPrefix(trimmed, "patch="); ok {
			requiredFiles = append(requiredFiles, after)
		} else {
			slog.Debug("Ignoring unexpected line from rpmspec", "line", trimmed)
		}
	}

	// Validate that we have what we were expecting.
	if name == "" || epoch == "" || version == "" || release == "" {
		return nil, fmt.Errorf(
			"failed to parse spec '%s': "+"missing required fields (name: %q, epoch: %q, version: %q, release: %q)",
			specPath, name, epoch, version, release,
		)
	}

	// Construct a version.
	versionObject, err := NewVersionFromEVR(epoch, version, release)
	if err != nil {
		return nil, fmt.Errorf("failed to create version from EVR:\n%w", err)
	}

	slog.Debug("Queried spec", "specPath", specPath, "version", version)

	return &SpecInfo{
		Name:          name,
		Version:       *versionObject,
		NEVR:          FormatNEVR(name, versionObject),
		RequiredFiles: requiredFiles,
	}, nil
}

func (q *SpecQuerier) composeBinaryRpmspecCmdline(specPath string) (result []string) {
	specDirPath := filepath.Dir(specPath)

	// Compose command without --srpm to query all binary subpackages.
	// Use a separator to delimit subpackage boundaries in output.
	result = []string{
		"rpmspec",
		"-q",
		"-D", "_sourcedir " + specDirPath,
		"-D", "_specdir " + specDirPath,
		"-D", "with_check 0",
		"--queryformat",
		"pkg_name=%{name}\npkg_epoch=%{epoch}\npkg_version=%{version}\npkg_release=%{release}\n" +
			subpackageSeparator + "\n",
	}

	for _, name := range q.buildOptions.With {
		result = append(result, "--with", name)
	}

	for _, name := range q.buildOptions.Without {
		result = append(result, "--without", name)
	}

	for key, value := range q.buildOptions.Defines {
		result = append(result, "-D", fmt.Sprintf("%s %s", key, value))
	}

	result = append(result, specPath)

	return result
}

func parseBinaryRpmspecOutput(specPath, output string) ([]PackageNEVR, error) {
	// Split the output into blocks separated by the subpackage separator.
	blocks := strings.Split(output, subpackageSeparator)
	subpackages := make([]PackageNEVR, 0, len(blocks))

	for _, block := range blocks {
		pkg, isEmpty, err := parseSubpackageBlock(specPath, block)
		if err != nil {
			return nil, err
		}

		if isEmpty {
			continue
		}

		subpackages = append(subpackages, *pkg)
	}

	return subpackages, nil
}

//nolint:cyclop // This function's complexity is due to the if/else-if cases for parsing, mirroring parseRpmspecOutput.
func parseSubpackageBlock(specPath, block string) (pkg *PackageNEVR, isEmpty bool, err error) {
	var name, epoch, version, release string

	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Ignore rpmspec warnings/errors intermixed with output.
		//nolint:nestif // Mirrors the structure in parseRpmspecOutput for consistency.
		if strings.HasPrefix(trimmed, "error: ") || strings.HasPrefix(trimmed, "warning: ") {
			slog.Debug("Ignoring rpmspec error in subpackage output", "line", trimmed)
		} else if after, ok := strings.CutPrefix(trimmed, "pkg_name="); ok {
			name = after
		} else if after, ok := strings.CutPrefix(trimmed, "pkg_epoch="); ok {
			if after == "(none)" {
				epoch = "0"
			} else {
				epoch = after
			}
		} else if after, ok := strings.CutPrefix(trimmed, "pkg_version="); ok {
			version = after
		} else if after, ok := strings.CutPrefix(trimmed, "pkg_release="); ok {
			release = after
		} else {
			slog.Debug("Ignoring unexpected line in subpackage output", "line", trimmed)
		}
	}

	// Skip empty blocks (e.g., trailing separator).
	if name == "" && epoch == "" && version == "" && release == "" {
		return nil, true, nil
	}

	if name == "" || epoch == "" || version == "" || release == "" {
		return nil, false, fmt.Errorf(
			"failed to parse subpackage from spec %#q: "+
				"missing required fields (name: %q, epoch: %q, version: %q, release: %q)",
			specPath, name, epoch, version, release,
		)
	}

	versionObject, err := NewVersionFromEVR(epoch, version, release)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create version for subpackage %#q:\n%w", name, err)
	}

	result := NewPackageNEVR(name, *versionObject)

	return &result, false, nil
}
