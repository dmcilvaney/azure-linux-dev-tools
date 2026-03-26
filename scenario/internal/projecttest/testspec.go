// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package projecttest

import (
	"fmt"
	"strings"
)

// TestSpecOption is a function that can be used to modify a [TestSpec] in-place.
type TestSpecOption func(*TestSpec)

// NoArch is a constant representing the "noarch" architecture for RPMs.
const NoArch = "noarch"

// TestSpec represents an RPM spec being composed for testing purposes.
type TestSpec struct {
	name        string
	version     string
	release     string
	epoch       string
	buildArch   string
	subpackages []string
}

// NewSpec creates a new [TestSpec] with the specified options.
func NewSpec(options ...TestSpecOption) *TestSpec {
	// Start with defaults.
	spec := &TestSpec{
		name:      "test-component",
		version:   "1.2.3",
		release:   "4.rel",
		buildArch: "",
	}

	for _, option := range options {
		option(spec)
	}

	return spec
}

// GetName returns the name of the component defined by the spec.
func (s *TestSpec) GetName() string {
	return s.name
}

// GetVersion returns the version of the component defined by the spec.
func (s *TestSpec) GetVersion() string {
	return s.version
}

// GetRelease returns the release of the component defined by the spec.
func (s *TestSpec) GetRelease() string {
	return s.release
}

// WithName sets the name of the component defined by the spec.
func WithName(name string) TestSpecOption {
	return func(s *TestSpec) {
		s.name = name
	}
}

// WithVersion sets the version of the component defined by the spec.
func WithVersion(version string) TestSpecOption {
	return func(s *TestSpec) {
		s.version = version
	}
}

// WithRelease sets the release of the component defined by the spec.
func WithRelease(release string) TestSpecOption {
	return func(s *TestSpec) {
		s.release = release
	}
}

// WithBuildArch sets the build architecture of the component defined by the spec.
func WithBuildArch(arch string) TestSpecOption {
	return func(s *TestSpec) {
		s.buildArch = arch
	}
}

// WithEpoch sets the epoch of the component defined by the spec.
func WithEpoch(epoch string) TestSpecOption {
	return func(s *TestSpec) {
		s.epoch = epoch
	}
}

// WithSubpackage adds a subpackage to the spec. The suffix is appended to the base
// name to form the subpackage name (e.g., suffix "devel" produces "name-devel").
func WithSubpackage(suffix string) TestSpecOption {
	return func(s *TestSpec) {
		s.subpackages = append(s.subpackages, suffix)
	}
}

// GetEpoch returns the epoch of the component defined by the spec.
func (s *TestSpec) GetEpoch() string {
	return s.epoch
}

// GetExpectedNEVR returns the expected NEVR string for the SRPM, following RPM convention:
// epoch 0 or empty produces "name-version-release", non-zero epoch produces "name-epoch:version-release".
func (s *TestSpec) GetExpectedNEVR() string {
	return s.formatNEVR(s.name)
}

// GetExpectedSubpackageNEVR returns the expected NEVR string for a subpackage with the given suffix.
func (s *TestSpec) GetExpectedSubpackageNEVR(suffix string) string {
	return s.formatNEVR(s.name + "-" + suffix)
}

func (s *TestSpec) formatNEVR(name string) string {
	if s.epoch != "" && s.epoch != "0" {
		return fmt.Sprintf("%s-%s:%s-%s", name, s.epoch, s.version, s.release)
	}

	return fmt.Sprintf("%s-%s-%s", name, s.version, s.release)
}

// Render generates the spec file content as a string.
func (s *TestSpec) Render() string {
	lines := []string{
		"Name: " + s.name,
		"Version: " + s.version,
		"Release: " + s.release,
	}

	if s.epoch != "" {
		lines = append(lines, "Epoch: "+s.epoch)
	}

	lines = append(lines, "Summary: A test component", "License: MIT")

	if s.buildArch != "" {
		lines = append(lines, "BuildArch: "+s.buildArch)
	}

	lines = append(lines, []string{
		"",
		"%description",
		"Test component for, you know, testing.",
	}...)

	for _, suffix := range s.subpackages {
		lines = append(lines, []string{
			"",
			"%package " + suffix,
			"Summary: " + suffix + " subpackage",
			"",
			"%description " + suffix,
			suffix + " subpackage for testing.",
		}...)
	}

	lines = append(lines, []string{
		"",
		"%build",
		"echo hello >file.txt",
		"",
		"%install",
		"mkdir -p %{buildroot}/%{_datadir}/test-component",
		"cp file.txt %{buildroot}/%{_datadir}/test-component/file.txt",
		"",
		"%files",
		"%{_datadir}/test-component",
	}...)

	for _, suffix := range s.subpackages {
		lines = append(lines, []string{
			"",
			"%files " + suffix,
		}...)
	}

	lines = append(lines, "")

	return strings.Join(lines, "\n")
}
