// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

var errTestCapture = errors.New("test diagnostic capture failed")

const (
	testStatusCompleted  = "completed"
	testStatusIncomplete = "incomplete"
	testStageSetup       = "setup"
)

type testReportError struct {
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message"`
}

type testArtifact struct {
	Format string `json:"format"`
	Plan   string `json:"plan,omitempty"`
	Path   string `json:"path"`
}

type testLog struct {
	Kind         string    `json:"kind"`
	Path         string    `json:"path"`
	Plan         string    `json:"plan,omitempty"`
	Test         string    `json:"test,omitempty"`
	Guest        *tmtGuest `json:"guest,omitempty"`
	SerialNumber *int      `json:"serialNumber,omitempty"`
}

type testInvocation struct {
	Name            string            `json:"name"`
	Runner          string            `json:"runner"`
	RunnerVersion   string            `json:"runnerVersion,omitempty"`
	Status          string            `json:"status"`
	ExitCode        *int              `json:"exitCode,omitempty"`
	Reports         []testArtifact    `json:"reports"`
	Logs            []testLog         `json:"logs"`
	NativeArtifacts []testArtifact    `json:"nativeArtifacts"`
	Errors          []testReportError `json:"errors"`
}

type componentTestReport struct {
	SchemaVersion int `json:"schemaVersion"`
	Subject       struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"subject"`
	Status       string            `json:"status"`
	ArtifactRoot string            `json:"artifactRoot,omitempty"`
	Logs         []testLog         `json:"logs"`
	Invocations  []testInvocation  `json:"invocations"`
	Errors       []testReportError `json:"errors"`
}

func newComponentTestReport(name string, tests []projectconfig.ResolvedTest) *componentTestReport {
	report := &componentTestReport{
		SchemaVersion: 1,
		Status:        testStatusIncomplete,
		Logs:          []testLog{},
		Invocations:   make([]testInvocation, len(tests)),
		Errors:        []testReportError{},
	}
	report.Subject.Type = "component"

	report.Subject.Name = name
	for index, test := range tests {
		report.Invocations[index] = testInvocation{
			Name: test.Name, Runner: tmtProgram, Status: "not-run",
			Reports: []testArtifact{}, Logs: []testLog{}, NativeArtifacts: []testArtifact{}, Errors: []testReportError{},
		}
	}

	return report
}

func executeComponentTMTTests(
	env *azldev.Env, name string, workDir string, tests []projectconfig.ResolvedTest, settings tmtRunSettings,
) (report *componentTestReport, err error) {
	report = newComponentTestReport(name, tests)
	if env.DryRun() {
		report.Status = "dry-run"
		for index := range report.Invocations {
			report.Invocations[index].Status = "dry-run"
		}

		return report, nil
	}

	errorStage := testStageSetup

	defer func() {
		if err != nil {
			report.Errors = append(report.Errors, testReportError{Stage: errorStage, Message: err.Error()})
		}
	}()

	workDir, err = componentTMTWorkDir(env, workDir)
	if err != nil {
		return report, err
	}

	if err = env.FS().MkdirAll(workDir, fileperms.PrivateDir); err != nil {
		return report, fmt.Errorf("create TMT work directory:\n%w", err)
	}

	report.ArtifactRoot, err = fileutils.MkdirTemp(env.FS(), workDir, "azldev-test-")
	if err != nil {
		return report, fmt.Errorf("create test artifact directory:\n%w", err)
	}

	slog.Info("Retaining test artifacts", "path", report.ArtifactRoot)
	settings.SetupLog = filepath.Join(report.ArtifactRoot, "setup.log")

	defer func() {
		if path, logErr := retainedFile(env.FS(), report.ArtifactRoot, settings.SetupLog); logErr == nil {
			report.Logs = append(report.Logs, testLog{Kind: testStageSetup, Path: path})
		} else if !errors.Is(logErr, os.ErrNotExist) {
			err = errors.Join(err, logErr)
			report.Status = testStatusIncomplete
		}
	}()

	_, settings.TMTProgramPath, err = prepareTMTEnvironment(
		env, workDir, settings.Provision, settings.SourceDir != "", settings.SetupLog,
	)
	if err != nil {
		return report, err
	}

	errorStage = ""

	for index, test := range tests {
		invocation := &report.Invocations[index]

		settings.WorkDir = filepath.Join(report.ArtifactRoot, "invocations", fmt.Sprintf("%03d-%s", index+1, test.Name))
		if err := executeTMTInvocation(env, report.ArtifactRoot, settings, test, invocation); err != nil {
			if index == len(tests)-1 && invocation.Status == testStatusCompleted {
				report.Status = testStatusCompleted
			}

			return report, fmt.Errorf("TMT test %#q:\n%w", test.Name, err)
		}
	}

	report.Status = testStatusCompleted

	return report, nil
}

func executeTMTInvocation(
	env *azldev.Env, root string, settings tmtRunSettings, test projectconfig.ResolvedTest, invocation *testInvocation,
) error {
	invocation.Status = testStatusIncomplete

	runErr := runOneTMTTest(env, test, settings, invocation)
	if runErr != nil {
		stage := ""
		if errors.Is(runErr, errTestCapture) {
			stage = "collection"
		} else if invocation.RunnerVersion == "" {
			stage = testStageSetup
		}

		invocation.Errors = append(invocation.Errors, testReportError{Stage: stage, Message: runErr.Error()})
	}

	collectionErr := collectTMTArtifacts(env, root, settings, invocation)
	if errors.Is(runErr, errTestCapture) {
		invocation.Status = testStatusIncomplete
	}

	return errors.Join(runErr, collectionErr)
}

// Both process streams may write concurrently. Remember capture errors even
// when exec returns an ExitError instead of its stream-copy error.
type testTranscript struct {
	mu     sync.Mutex
	writer io.Writer
	err    error
}

func (w *testTranscript) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	count, err := w.writer.Write(data)
	if err == nil && count != len(data) {
		err = io.ErrShortWrite
	}

	w.err = errors.Join(w.err, err)

	return count, err
}

//nolint:wrapcheck // The deferred wrapper adds command/transcript context to returned errors.
func runCapturedTMTCommand(
	env *azldev.Env, command *exec.Cmd, logPath string, captureOutput bool,
) (output string, exitCode *int, err error) {
	if env.DryRun() {
		return "", nil, nil
	}

	transcript := io.Discard

	if logPath != "" {
		file, openErr := env.FS().OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileperms.PrivateFile)
		if openErr != nil {
			return "", nil, fmt.Errorf("open command transcript %#q:\n%w", logPath, errors.Join(errTestCapture, openErr))
		}

		defer func() {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, errTestCapture, closeErr)
			}
		}()

		transcript = file
	}

	log := &testTranscript{writer: transcript}

	defer func() {
		if log.err != nil {
			err = errors.Join(err, errTestCapture, log.err)
		}

		if err != nil {
			err = fmt.Errorf("command %q (transcript %#q):\n%w", command.Args, logPath, err)
		}
	}()

	if _, err = fmt.Fprintf(log, "$ %q\n", command.Args); err != nil {
		return "", nil, err
	}

	stdout := env.ReportFile()
	if env.DefaultReportFormat() == azldev.ReportFormatJSON {
		stdout = os.Stderr
	}

	command.Stderr = io.MultiWriter(log, os.Stderr)
	if !captureOutput {
		command.Stdout = io.MultiWriter(log, stdout)
	}

	wrapped, err := env.Command(command)
	if err != nil {
		return "", nil, err
	}

	if captureOutput {
		output, err = wrapped.RunAndGetOutput(env)
		_, writeErr := io.WriteString(log, output)
		err = errors.Join(err, writeErr)
	} else {
		err = wrapped.Run(env)
	}

	if command.ProcessState != nil && command.ProcessState.Exited() {
		code := command.ProcessState.ExitCode()
		exitCode = &code
	}

	return output, exitCode, err
}
