// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build scenario

package scenario_tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/scenario/internal/projecttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localComponentConfig creates a component config for a local spec at the standard test path.
func localComponentConfig(name string, overlays ...projectconfig.ComponentOverlay) *projectconfig.ComponentConfig {
	return &projectconfig.ComponentConfig{
		Name: name,
		Spec: projectconfig.SpecSource{
			SourceType: projectconfig.SpecSourceTypeLocal,
			Path:       filepath.Join("specs", name, name+".spec"),
		},
		Overlays: overlays,
	}
}

func TestRenderSimpleLocalSpec(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	spec := projecttest.NewSpec(
		projecttest.WithName("test-render"),
		projecttest.WithVersion("1.0.0"),
		projecttest.WithRelease("1%{?dist}"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.AddComponent(localComponentConfig("test-render")),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "render", "test-render", "-o", "project/SPECS"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	// Verify JSON output reports success.
	output := results.GetJSONResult()
	require.Len(t, output, 1, "Expected one component in the output")
	assert.Equal(t, "test-render", output[0]["component"])
	assert.Equal(t, "ok", output[0]["status"],
		"Simple spec should render without warnings when rpmautospec is installed")

	// Verify rendered spec file exists with expected content.
	renderedSpecPath := results.GetProjectOutputPath("SPECS", "test-render", "test-render.spec")
	require.FileExists(t, renderedSpecPath)

	content, err := os.ReadFile(renderedSpecPath)
	require.NoError(t, err)

	contentStr := string(content)
	assert.Contains(t, contentStr, "Name: test-render")
	assert.Contains(t, contentStr, "Version: 1.0.0")
}

func TestRenderWithOverlayApplied(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	spec := projecttest.NewSpec(
		projecttest.WithName("test-overlay"),
		projecttest.WithVersion("2.0.0"),
		projecttest.WithRelease("1%{?dist}"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.AddComponent(localComponentConfig("test-overlay",
			projectconfig.ComponentOverlay{
				Type:        projectconfig.ComponentOverlayAddSpecTag,
				Description: "Add test build dependency",
				Tag:         "BuildRequires",
				Value:       "test-overlay-dep",
			},
		)),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "render", "test-overlay", "-o", "project/SPECS"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	// Verify success.
	output := results.GetJSONResult()
	require.Len(t, output, 1)
	assert.Equal(t, "ok", output[0]["status"], "Spec should render as ok when rpmautospec is installed")

	// Verify the overlay was applied — the rendered spec should contain the added tag.
	renderedSpecPath := results.GetProjectOutputPath("SPECS", "test-overlay", "test-overlay.spec")
	require.FileExists(t, renderedSpecPath)

	content, err := os.ReadFile(renderedSpecPath)
	require.NoError(t, err)

	assert.Contains(t, string(content), "BuildRequires: test-overlay-dep",
		"Overlay should have added BuildRequires tag to rendered spec")
}

func TestRenderWithPatchSidecar(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	spec := projecttest.NewSpec(
		projecttest.WithName("test-patch"),
		projecttest.WithVersion("1.0.0"),
		projecttest.WithRelease("1%{?dist}"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	patchContent := `--- a/file.txt
+++ b/file.txt
@@ -1 +1 @@
-old
+new
`

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.AddComponent(localComponentConfig("test-patch",
			projectconfig.ComponentOverlay{
				Type:        projectconfig.ComponentOverlayAddPatch,
				Description: "Add test patch",
				Source:      "patches/fix-stuff.patch",
			},
		)),
		projecttest.AddFile("patches/fix-stuff.patch", patchContent),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "render", "test-patch", "-o", "project/SPECS"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	// Verify success.
	output := results.GetJSONResult()
	require.Len(t, output, 1)
	assert.Equal(t, "ok", output[0]["status"], "Spec should render as ok when rpmautospec is installed")

	// Verify the patch file is in the rendered output.
	patchPath := results.GetProjectOutputPath("SPECS", "test-patch", "fix-stuff.patch")
	require.FileExists(t, patchPath, "Patch sidecar should be in rendered output")

	// Verify the spec references the patch.
	renderedSpecPath := results.GetProjectOutputPath("SPECS", "test-patch", "test-patch.spec")
	content, err := os.ReadFile(renderedSpecPath)
	require.NoError(t, err)

	assert.Contains(t, string(content), "fix-stuff.patch",
		"Rendered spec should reference the added patch")
}

func TestRenderStaleCleanup(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	spec := projecttest.NewSpec(
		projecttest.WithName("keep-me"),
		projecttest.WithVersion("1.0.0"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	// Pre-populate a stale SPECS directory alongside the real component.
	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(spec),
		projecttest.AddComponent(localComponentConfig("keep-me")),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
		projecttest.AddFile("SPECS/stale-component/RENDER_FAILED", "Rendering failed.\n"),
	)

	results := projecttest.NewProjectTest(
		project,
		// Render all with -a to trigger stale cleanup.
		[]string{"component", "render", "-a", "-o", "project/SPECS", "--clean-stale"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	// Verify the kept component was rendered.
	output := results.GetJSONResult()
	require.Len(t, output, 1)
	assert.Equal(t, "keep-me", output[0]["component"])

	// Verify the stale directory was cleaned up.
	stalePath := results.GetProjectOutputPath("SPECS", "stale-component")
	assert.NoDirExists(t, stalePath, "Stale component directory should have been removed")

	// Verify the kept component still exists.
	keptPath := results.GetProjectOutputPath("SPECS", "keep-me")
	assert.DirExists(t, keptPath, "Rendered component directory should still exist")
}

// TestRenderSpecWithUndefinedMacros verifies that a spec using macros not available
// on the host (like %gometa for golang packages) renders with "warning" status rather
// than failing. The spec should still be present in the output, just with unexpanded
// macros. This currently affects ~430 of ~7k packages in the real azurelinux project.
//
// TODO(bn-706a): Once mock chroot fallback is implemented, this test should assert
// "ok" status instead of "warning", since mock will have all macro packages available.
func TestRenderSpecWithUndefinedMacros(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	// Simulate a golang-style spec that uses %gometa — a macro defined by
	// go-rpm-macros which is typically not installed on the host.
	// Must set %goipath and %forgeurl before %gometa (required by the macro).
	goSpecContent := `%global goipath         go.uber.org/atomic
%global forgeurl        https://github.com/uber-go/atomic
Version:                1.11.0

%gometa

%global common_description %{expand:
Test golang package using gometa macro.}

Name:           golang-example
Release:        %autorelease
Summary:        Example golang package
License:        MIT

%description
%{common_description}

%prep
%goprep

%build
%gobuild

%install
%goinstall

%files
%license LICENSE

%changelog
%autochangelog
`

	project := projecttest.NewDynamicTestProject(
		projecttest.AddComponent(localComponentConfig("golang-example")),
		// Write the custom spec content directly via AddFile since AddSpec's
		// TestSpec renderer doesn't support %gometa.
		projecttest.AddFile("specs/golang-example/golang-example.spec", goSpecContent),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "render", "golang-example", "-o", "project/SPECS"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	output := results.GetJSONResult()
	require.Len(t, output, 1)

	// With mock fallback working (azl4 chroot has go-srpm-macros + rpmautospec),
	// rpmautospec should succeed. Spectool may still fail for complex golang specs
	// so status may be "warning" (filtering skipped) rather than "ok".
	assert.Contains(t, []string{"ok", "warning"}, output[0]["status"],
		"Spec with golang macros should render via mock fallback (warning acceptable if spectool fails)")

	// Verify mock fallback WAS triggered (host rpmautospec can't parse %gometa).
	assert.Contains(t, results.GetStderr(), "Initializing mock chroot",
		"Mock fallback should be triggered for specs with undefined host macros")

	// The spec file should exist in the output.
	renderedSpecPath := results.GetProjectOutputPath("SPECS", "golang-example", "golang-example.spec")
	require.FileExists(t, renderedSpecPath,
		"Spec should be rendered via mock fallback")

	// The rendered spec should have rpmautospec headers (macros were processed).
	content, err := os.ReadFile(renderedSpecPath)
	require.NoError(t, err)

	// rpmautospec should have added its header with the %define for autorelease.
	assert.Contains(t, string(content), "## START: Set by rpmautospec",
		"rpmautospec should have processed the spec")
	// %autochangelog should be expanded to real changelog entries.
	assert.NotContains(t, string(content), "%autochangelog",
		"%%autochangelog should be expanded to real entries")
}

// TestRenderMultipleComponentsParallel verifies that rendering two or more
// components in a single invocation works correctly. This exercises the batch
// mock processing path with parallel bash jobs.
func TestRenderMultipleComponentsParallel(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	specA := projecttest.NewSpec(
		projecttest.WithName("comp-alpha"),
		projecttest.WithVersion("1.0.0"),
		projecttest.WithRelease("1%{?dist}"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	specB := projecttest.NewSpec(
		projecttest.WithName("comp-beta"),
		projecttest.WithVersion("2.0.0"),
		projecttest.WithRelease("%autorelease"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(specA),
		projecttest.AddSpec(specB),
		projecttest.AddComponent(localComponentConfig("comp-alpha")),
		projecttest.AddComponent(localComponentConfig("comp-beta")),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "render", "-a", "-o", "project/SPECS", "--clean-stale"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	output := results.GetJSONResult()
	require.Len(t, output, 2, "Expected two components in the output")

	// Build a map for easier assertion.
	resultMap := make(map[string]map[string]interface{}, len(output))
	for _, entry := range output {
		name, _ := entry["component"].(string)
		resultMap[name] = entry
	}

	// Both should succeed.
	assert.Equal(t, "ok", resultMap["comp-alpha"]["status"],
		"comp-alpha should render ok")
	assert.Equal(t, "ok", resultMap["comp-beta"]["status"],
		"comp-beta should render ok")

	// Verify both rendered specs exist.
	specAlphaPath := results.GetProjectOutputPath("SPECS", "comp-alpha", "comp-alpha.spec")
	require.FileExists(t, specAlphaPath)

	specBetaPath := results.GetProjectOutputPath("SPECS", "comp-beta", "comp-beta.spec")
	require.FileExists(t, specBetaPath)

	// comp-beta uses %autorelease, so rpmautospec should have processed it.
	betaContent, err := os.ReadFile(specBetaPath)
	require.NoError(t, err)
	assert.Contains(t, string(betaContent), "## START: Set by rpmautospec",
		"rpmautospec should have expanded %%autorelease for comp-beta")
}

// TestRenderBrokenSpecWithGoodSpec verifies that a malformed spec produces an
// error result while a valid spec in the same batch still renders successfully.
// This exercises the Python script's error handling in a real mock chroot.
func TestRenderBrokenSpecWithGoodSpec(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long test")
	}

	goodSpec := projecttest.NewSpec(
		projecttest.WithName("good-pkg"),
		projecttest.WithVersion("1.0.0"),
		projecttest.WithRelease("1%{?dist}"),
		projecttest.WithBuildArch(projecttest.NoArch),
	)

	project := projecttest.NewDynamicTestProject(
		projecttest.AddSpec(goodSpec),
		projecttest.AddComponent(localComponentConfig("good-pkg")),
		// Add a broken spec as a raw file — not valid RPM spec syntax.
		projecttest.AddFile("specs/broken-pkg/broken-pkg.spec", "this is not a valid spec file\n"),
		projecttest.AddComponent(localComponentConfig("broken-pkg")),
		projecttest.UseTestDefaultConfigs(),
		projecttest.WithGitRepo(),
	)

	results := projecttest.NewProjectTest(
		project,
		[]string{"component", "render", "-a", "-o", "project/SPECS", "--clean-stale"},
		projecttest.WithTestDefaultConfigs(),
	).RunInContainer(t)

	output := results.GetJSONResult()
	require.Len(t, output, 2, "Expected two components in the output")

	// Build a map for easier assertion.
	resultMap := make(map[string]map[string]interface{}, len(output))
	for _, entry := range output {
		name, _ := entry["component"].(string)
		resultMap[name] = entry
	}

	// Good spec should succeed despite the broken one in the same batch.
	assert.Equal(t, "ok", resultMap["good-pkg"]["status"],
		"good-pkg should render ok even when another component fails")

	goodSpecPath := results.GetProjectOutputPath("SPECS", "good-pkg", "good-pkg.spec")
	require.FileExists(t, goodSpecPath)

	// Broken spec should produce an error status.
	assert.Equal(t, "error", resultMap["broken-pkg"]["status"],
		"broken-pkg should report error for malformed spec")

	// Error marker file should be written for the broken component.
	markerPath := results.GetProjectOutputPath("SPECS", "broken-pkg", "RENDER_FAILED")
	require.FileExists(t, markerPath, "RENDER_FAILED marker should exist for broken component")
}
