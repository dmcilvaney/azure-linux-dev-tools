// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rpm

import "fmt"

// PackageNEVR represents the fully-resolved identity of an RPM package.
type PackageNEVR struct {
	Name    string  `json:"name"`
	Version Version `json:"version"`
	NEVR    string  `json:"nevr"`
}

// FormatNEVR produces a formatted NEVR string from a package name and [Version].
// When epoch is 0, the format is "name-version-release" (matching RPM convention of omitting zero epoch).
// When epoch is greater than 0, the format is "name-epoch:version-release".
func FormatNEVR(name string, version *Version) string {
	if version.Epoch() > 0 {
		return fmt.Sprintf("%s-%d:%s-%s", name, version.Epoch(), version.Version(), version.Release())
	}

	return fmt.Sprintf("%s-%s-%s", name, version.Version(), version.Release())
}

// NewPackageNEVR creates a new [PackageNEVR] from a name and [Version], automatically computing the
// formatted NEVR string.
func NewPackageNEVR(name string, version Version) PackageNEVR {
	return PackageNEVR{
		Name:    name,
		Version: version,
		NEVR:    FormatNEVR(name, &version),
	}
}
