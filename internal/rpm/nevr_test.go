// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rpm_test

import (
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/rpm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatNEVR(t *testing.T) {
	tests := []struct {
		name         string
		packageName  string
		epoch        string
		version      string
		release      string
		expectedNEVR string
	}{
		{
			name:         "zero epoch omitted",
			packageName:  "curl",
			epoch:        "0",
			version:      "7.88.1",
			release:      "2.azl3",
			expectedNEVR: "curl-7.88.1-2.azl3",
		},
		{
			name:         "empty epoch treated as zero",
			packageName:  "curl",
			epoch:        "",
			version:      "7.88.1",
			release:      "2.azl3",
			expectedNEVR: "curl-7.88.1-2.azl3",
		},
		{
			name:         "non-zero epoch included",
			packageName:  "vim",
			epoch:        "2",
			version:      "9.0.1000",
			release:      "1.azl3",
			expectedNEVR: "vim-2:9.0.1000-1.azl3",
		},
		{
			name:         "large epoch value",
			packageName:  "pkg",
			epoch:        "999",
			version:      "1.0",
			release:      "1.azl3",
			expectedNEVR: "pkg-999:1.0-1.azl3",
		},
		{
			name:         "package name with hyphen",
			packageName:  "curl-devel",
			epoch:        "0",
			version:      "7.88.1",
			release:      "2.azl3",
			expectedNEVR: "curl-devel-7.88.1-2.azl3",
		},
		{
			name:         "package name with hyphen and non-zero epoch",
			packageName:  "curl-libs",
			epoch:        "1",
			version:      "7.88.1",
			release:      "2.azl3",
			expectedNEVR: "curl-libs-1:7.88.1-2.azl3",
		},
		{
			name:         "version with dots and alpha suffix",
			packageName:  "python3",
			epoch:        "0",
			version:      "3.12.0~b4",
			release:      "1.azl3",
			expectedNEVR: "python3-3.12.0~b4-1.azl3",
		},
		{
			name:         "complex release string",
			packageName:  "kernel",
			epoch:        "0",
			version:      "6.1.58",
			release:      "1.cm2.rc1",
			expectedNEVR: "kernel-6.1.58-1.cm2.rc1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version, err := rpm.NewVersionFromEVR(test.epoch, test.version, test.release)
			require.NoError(t, err)

			result := rpm.FormatNEVR(test.packageName, version)
			assert.Equal(t, test.expectedNEVR, result)
		})
	}
}

func TestNewPackageNEVR(t *testing.T) {
	tests := []struct {
		name         string
		packageName  string
		epoch        string
		version      string
		release      string
		expectedNEVR string
	}{
		{
			name:         "zero epoch",
			packageName:  "curl",
			epoch:        "0",
			version:      "7.88.1",
			release:      "2.azl3",
			expectedNEVR: "curl-7.88.1-2.azl3",
		},
		{
			name:         "non-zero epoch",
			packageName:  "vim",
			epoch:        "2",
			version:      "9.0.1000",
			release:      "1.azl3",
			expectedNEVR: "vim-2:9.0.1000-1.azl3",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version, err := rpm.NewVersionFromEVR(test.epoch, test.version, test.release)
			require.NoError(t, err)

			pkg := rpm.NewPackageNEVR(test.packageName, *version)

			assert.Equal(t, test.packageName, pkg.Name)
			assert.Equal(t, test.expectedNEVR, pkg.NEVR)
			assert.Equal(t, test.version, pkg.Version.Version())
			assert.Equal(t, test.release, pkg.Version.Release())
		})
	}
}
