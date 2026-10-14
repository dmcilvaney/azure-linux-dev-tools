// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package magescenario

import (
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/buildtestenv"
	"github.com/microsoft/azure-linux-dev-tools/magefiles/magebuild"
	"github.com/microsoft/azure-linux-dev-tools/magefiles/mageutil"
)

var ErrScenario = errors.New("scenario test failed")

// Scenario runs the scenario tests.
func Scenario() error {
	startTime := time.Now()

	mageutil.MagePrintln(mageutil.MsgStart, "Starting scenario tests...")

	mg.SerialDeps(mg.F(mageScenarioCommon, false))

	mageutil.MagePrintf(mageutil.MsgSuccess, "Scenario tests complete: %v.\n", time.Since(startTime))

	return nil
}

// ScenarioUpdate updates snapshots and runs the scenario tests.
func ScenarioUpdate() error {
	mageutil.MagePrintln(mageutil.MsgStart, "Updating scenario snapshots...")

	mg.SerialDeps(mg.F(mageScenarioCommon, true))

	mageutil.MagePrintln(mageutil.MsgSuccess, "Scenario snapshots updated.")

	return nil
}

// E2E runs the end-to-end tests. These are tagged with the `e2e` build tag
// (separate from `scenario`) so that they do not run as part of `mage scenario`,
// `mage scenarioUpdate`, or `mage all` — they are intentionally heavier
// (network access, large clones, full mock pipelines) and meant to be run
// from CI on a dedicated job.
func E2E() error {
	startTime := time.Now()

	mageutil.MagePrintln(mageutil.MsgStart, "Starting end-to-end tests...")

	err := runScenarioGoTest(scenarioGoTestOptions{
		label: "end-to-end",
		// Use the dedicated 'e2e' tag so that scenario tests are excluded.
		buildTag: "e2e",
		// E2E tests are individually long-running (clone hundreds of MB,
		// invoke mock thousands of times). A full render against the
		// upstream repo runs ~45m on default GitHub runners; give it
		// roughly double for headroom.
		timeout: "90m",
		// Limit to './scenario' (not './scenario/...') so the framework's own
		// internal-package tests — already covered by 'mage scenario' — are
		// not duplicated here.
		packagePattern: "./scenario",
	})
	if err != nil {
		return err
	}

	mageutil.MagePrintf(mageutil.MsgSuccess, "End-to-end tests complete: %v.\n", time.Since(startTime))

	return nil
}

// E2EImage runs only the Azure Linux vm-base image build test.
func E2EImage() error {
	startTime := time.Now()

	mageutil.MagePrintln(mageutil.MsgStart, "Starting Azure Linux image-build end-to-end test...")

	err := runScenarioGoTest(scenarioGoTestOptions{
		label:          "image-build end-to-end",
		buildTag:       "e2e",
		timeout:        "25m",
		packagePattern: "./scenario",
		testSelector:   "^TestAzureLinuxVMBaseImageBuild$",
	})
	if err != nil {
		return err
	}

	mageutil.MagePrintf(
		mageutil.MsgSuccess,
		"Azure Linux image-build end-to-end test complete: %v.\n",
		time.Since(startTime),
	)

	return nil
}

// E2ETMT runs only the temporarily quarantined virtual TMT regression.
func E2ETMT() error {
	startTime := time.Now()

	mageutil.MagePrintln(mageutil.MsgStart, "Starting virtual TMT end-to-end test...")

	err := runScenarioGoTest(scenarioGoTestOptions{
		label:          "virtual TMT end-to-end",
		buildTag:       "e2e",
		timeout:        "45m",
		packagePattern: "./scenario",
		testSelector:   "^TestTMTVirtualProvisioning$",
		additionalEnv: map[string]string{
			"TESTING_ENABLE_TMT_VIRTUAL_E2E": "1",
		},
	})
	if err != nil {
		return err
	}

	mageutil.MagePrintf(
		mageutil.MsgSuccess,
		"Virtual TMT end-to-end test complete: %v.\n",
		time.Since(startTime),
	)

	return nil
}

func mageScenarioCommon(doSnapshotUpdate bool) error {
	options := scenarioGoTestOptions{
		label:            "scenario",
		buildTag:         "scenario",
		timeout:          "60m",
		packagePattern:   "./scenario/...",
		doSnapshotUpdate: doSnapshotUpdate,
	}

	if doSnapshotUpdate {
		mageutil.MagePrintln(mageutil.MsgInfo, "Will update snapshots.")
	}

	return runScenarioGoTest(options)
}

// scenarioGoTestOptions parameterizes the shared 'go test' invocation used by
// both [Scenario] / [ScenarioUpdate] and [E2E]. They run identical
// build/test pipelines but differ in build tag, timeout, package pattern, and
// snapshot-update behavior.
type scenarioGoTestOptions struct {
	// label is a short human-readable name for the tier ("scenario" or
	// "end-to-end") used in progress and error messages so that CI logs
	// clearly identify which suite is running/failed.
	label string
	// buildTag is the Go build tag passed via '-tags='. Files in the scenario
	// package have either '//go:build scenario' or '//go:build e2e' (with
	// 'setup_test.go' widened to 'scenario || e2e' so [TestMain] is shared).
	buildTag string
	// timeout is the value passed via '-timeout=' to 'go test'.
	timeout string
	// packagePattern is the package selector passed to 'go test', e.g.
	// './scenario' or './scenario/...'.
	packagePattern string
	// doSnapshotUpdate, when true, sets the env var that puts the snapshot
	// helper into update mode. Only meaningful for the scenario tier.
	doSnapshotUpdate bool
	// testSelector, when set, is passed to 'go test -run' to execute one
	// focused test without running the rest of the selected tier.
	testSelector string
	// additionalEnv adds target-specific environment variables to the test process.
	additionalEnv map[string]string
}

func runScenarioGoTest(options scenarioGoTestOptions) error {
	mg.SerialDeps(magebuild.Build)

	mageutil.MagePrintf(mageutil.MsgStart, "Running %s tests...\n", options.label)

	//nolint:lll // Can't really break up the long URI.
	// Add workaround for go-snaps disablement of color based on $_.
	// Reference: https://github.com/gkampitakis/go-snaps/blob/e004bc15e5166169d41a9d082967275fffb9d2b4/internal/colors/colors.go#L35
	if os.Getenv(mageutil.MageColorEnableEnvVar) == "true" {
		os.Setenv("_", "")
	}

	// mage knows the exact path to the azldev binary, ensure we use it.
	env := map[string]string{
		buildtestenv.TestingAzldevBinPathEnvVar: path.Join(mageutil.BinDir(), "azldev"),
	}
	for name, value := range options.additionalEnv {
		env[name] = value
	}

	if options.doSnapshotUpdate {
		env[buildtestenv.TestingUpdateSnapshotsEnvVar] = buildtestenv.TestingUpdateSnapshotsEnvValue
	}

	// '-count=1' causes the cache to be ignored for scenario tests so they always re-run (it would only rebuild if
	// we changed the test code itself, not the application code).
	args := []string{
		"test",
		"-tags=" + options.buildTag,
		"-timeout=" + options.timeout,
		"-count=1",
	}
	if options.testSelector != "" {
		args = append(args, "-run="+options.testSelector)
	}

	args = append(args, options.packagePattern)

	output, err := sh.OutputWith(env, mg.GoCmd(), args...)
	if err != nil {
		titledLabel := strings.ToUpper(options.label[:1]) + options.label[1:]
		mageutil.MagePrintf(mageutil.MsgError, "%s test failed; details follow.\n", titledLabel)

		for _, line := range strings.Split(output, "\n") {
			mageutil.MagePrintln(mageutil.MsgInfo, line)
		}

		return mageutil.PrintAndReturnError(titledLabel+" test failed (see diagnostic output above).", ErrScenario, err)
	}

	return nil
}
