// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reportTestEnv(t *testing.T) *testutils.TestEnv {
	t.Helper()
	env := newTMTTestEnv(t, nil)
	env.Env.SetDefaultReportFormat(azldev.ReportFormatJSON)
	env.CmdFactory.RegisterCommandInSearchPath("python3")

	for path, content := range map[string]string{
		testImagePath: "image", testRPMPath: "rpm", "/source/.fmf/version": "1\n",
	} {
		require.NoError(t, fileutils.WriteFile(env.TestFS, path, []byte(content), fileperms.PrivateFile))
	}

	env.CmdFactory.RunAndGetOutputHandler = func(cmd *exec.Cmd) (string, error) {
		_, err := io.WriteString(cmd.Stderr, "plan export diagnostic\n")

		return `[{"name":"/plans/all"}]`, err
	}

	return env
}

func configureReportTestEnv(t *testing.T, env *testutils.TestEnv, filesystem opctx.FS, dryRun bool) {
	t.Helper()

	options := azldev.NewEnvOptions()
	options.ProjectDir, options.Config = testProjectDir, env.Config
	options.Interfaces = env.TestInterfaces
	options.Interfaces.FileSystemFactory = reportFSFactory{filesystem}
	options.EventListener = env.EventListener
	options.DryRunnable = azldev.NewAppDryRunnable(dryRun)
	env.Env = azldev.NewEnv(t.Context(), options)
	env.Env.SetDefaultReportFormat(azldev.ReportFormatJSON)
}

func executeReportTest(t *testing.T, env *testutils.TestEnv) (componentTestReport, error) {
	t.Helper()

	var output strings.Builder
	env.Env.SetReportFile(&output)

	cmd := NewComponentTestCmd()
	cmd.SetArgs([]string{
		testTMTComponentName, "--image-path", testImagePath, "--rpm", testRPMPath,
		"--source-dir", "/source", "--work-dir", "/work", "--plan", "/plans/all",
	})
	err := cmd.ExecuteContext(env.Env)

	var report componentTestReport

	decoder := json.NewDecoder(strings.NewReader(output.String()))
	require.NoError(t, decoder.Decode(&report), "stdout: %s", output.String())
	require.ErrorIs(t, decoder.Decode(new(any)), io.EOF, "exactly one JSON document")

	return report, err
}

func writeTMTFixture(t *testing.T, env *testutils.TestEnv, native string, outcome string, incomplete string) {
	t.Helper()

	write := func(path, text string) {
		require.NoError(t, fileutils.WriteFile(env.TestFS, filepath.Join(native, path), []byte(text), fileperms.PrivateFile))
	}
	write("run.yaml", "plans:\n  - /plans/all\n")
	write("log.txt", "native diagnostics\n")

	for _, stage := range []string{"discover", "provision", "prepare", "execute", "finish", "report", "cleanup"} {
		status := "done"
		if stage == incomplete {
			status = "todo"
		}

		write("plans/all/"+stage+"/step.yaml", "status: "+status+"\n")
	}

	write("plans/all/execute/results.yaml", fmt.Sprintf(`
- name: /tests/example
  result: %s
  serial-number: 7
  guest: {name: client, role: client}
  log: [data/output.txt, data/output.txt]
  check:
    - name: dmesg
      result: pass
      log: [data/check.out]
  subresult:
    - name: /phase
      result: pass
      log: [data/subresult.txt]
      check:
        - name: nested
          result: pass
          log: [data/nested.out]
- name: /tests/example
  result: skip
  serial-number: 8
  guest: {name: server, role: server}
  log: []
`, outcome))

	for _, path := range []string{"output.txt", "check.out", "subresult.txt", "nested.out"} {
		write("plans/all/execute/data/"+path, "evidence: "+path)
	}
}

func mockJUnitExport(t *testing.T, env *testutils.TestEnv, cmd *exec.Cmd) {
	t.Helper()

	info, err := env.TestFS.Stat(cmd.Args[2])
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "export must receive the validated results file")
	require.NoError(t, fileutils.WriteFile(env.TestFS, cmd.Args[4], []byte(
		`<testsuites><testsuite><testcase name="client #7"/><testcase name="server #8"/></testsuite></testsuites>`,
	), fileperms.PrivateFile))
}

func mockTMTCommands(t *testing.T, env *testutils.TestEnv, run func(string) error) {
	t.Helper()

	env.CmdFactory.RunHandler = func(cmd *exec.Cmd) error {
		_, err := io.WriteString(cmd.Stdout, "noisy stdout\n")
		require.NoError(t, err)
		_, err = io.WriteString(cmd.Stderr, "noisy stderr\n")
		require.NoError(t, err)

		if filepath.Base(cmd.Args[0]) == tmtProgram {
			return run(cmd.Args[slices.Index(cmd.Args, "--id")+1])
		}

		if len(cmd.Args) > 1 && strings.HasSuffix(cmd.Args[1], "export-junit.py") {
			mockJUnitExport(t, env, cmd)
		}

		return nil
	}
}

func TestComponentTestReportLifecycle(t *testing.T) {
	for _, testCase := range []struct {
		name, outcome, incomplete, status string
		runFailure                        bool
	}{
		{"pass", "pass", "", "completed", false},
		{"assertion failure", "fail", "", "completed", true},
		{"native skip", "skip", "", "completed", true},
		{"native info", "info", "", "completed", false},
		{"native warn", "warn", "", "completed", true},
		{"native error", "error", "", "completed", true},
		{"pending even with done steps", "pending", "", "incomplete", true},
		{"interrupted passing result", "pass", "execute", "incomplete", true},
		{"prepare failure", "pending", "prepare", "incomplete", true},
		{"cleanup after passing tests", "pass", "cleanup", "incomplete", true},
		{"existing reporter failure", "pass", "report", "incomplete", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			env := reportTestEnv(t)
			mockTMTCommands(t, env, func(native string) error {
				writeTMTFixture(t, env, native, testCase.outcome, testCase.incomplete)

				if testCase.runFailure {
					return errors.New("observed runner failure")
				}

				return nil
			})

			report, err := executeReportTest(t, env)
			if testCase.runFailure {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, testCase.status, report.Status)
			require.Len(t, report.Invocations, 1)
			invocation := report.Invocations[0]
			assert.Equal(t, testCase.status, invocation.Status)
			assert.Nil(t, invocation.ExitCode, "mock did not observe a process exit")
			require.Len(t, invocation.Reports, 1)
			require.Len(t, invocation.Logs, 6)

			var paths []string
			for _, log := range invocation.Logs {
				paths = append(paths, log.Path)
				contents, readErr := fileutils.ReadFile(env.TestFS, filepath.Join(report.ArtifactRoot, log.Path))
				require.NoError(t, readErr)
				assert.NotEmpty(t, contents)

				if log.Kind != "runner" {
					require.NotNil(t, log.SerialNumber)
					assert.Equal(t, 7, *log.SerialNumber)
					require.NotNil(t, log.Guest)
					assert.Equal(t, "client", log.Guest.Name)
					assert.Equal(t, "/tests/example", log.Test)
				}
			}

			assert.True(t, slices.IsSorted(paths))
			require.Len(t, report.Logs, 1)
			setup, readErr := fileutils.ReadFile(env.TestFS, filepath.Join(report.ArtifactRoot, report.Logs[0].Path))
			require.NoError(t, readErr)
			assert.Contains(t, string(setup), "noisy stdout")
			assert.Contains(t, string(setup), "plan export diagnostic")
		})
	}
}

func TestComponentTestReportCollectionFailures(t *testing.T) {
	for _, testCase := range []struct {
		name, path, data string
		reports          int
	}{
		{"missing run state", "run.yaml", "", 0},
		{"missing results", "plans/all/execute/results.yaml", "", 0},
		{"malformed results", "plans/all/execute/results.yaml", "[not valid", 0},
		{"null results", "plans/all/execute/results.yaml", "null", 0},
		{"empty results", "plans/all/execute/results.yaml", "[]", 0},
		{"extra document", "plans/all/execute/results.yaml", "[]\n---\ninvalid: [", 0},
		{"invalid outcome", "plans/all/execute/results.yaml", "- name: /test\n  result: unknown\n", 0},
		{"escaping plan", "run.yaml", "plans: [/../../outside]\n", 0},
		{"colliding plan paths", "run.yaml", "plans: [/plans/all, /plans/a.b, /plans/a-b]\n", 1},
		{"malformed junit", "", "<testsuites>", 0},
		{"empty junit", "", "", 0},
		{"wrong junit count", "", "<testsuites/>", 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			env := reportTestEnv(t)
			env.CmdFactory.RunHandler = func(cmd *exec.Cmd) error {
				if filepath.Base(cmd.Args[0]) == tmtProgram {
					native := cmd.Args[slices.Index(cmd.Args, "--id")+1]
					writeTMTFixture(t, env, native, "fail", "")

					if testCase.path != "" {
						path := filepath.Join(native, testCase.path)
						if testCase.data == "" {
							require.NoError(t, env.TestFS.Remove(path))
						} else {
							require.NoError(t, fileutils.WriteFile(env.TestFS, path, []byte(testCase.data), fileperms.PrivateFile))
						}
					}

					return errors.New("execution failure")
				}

				if strings.HasSuffix(cmd.Args[1], "export-junit.py") {
					if testCase.path != "" {
						mockJUnitExport(t, env, cmd)
					} else {
						require.NoError(t, fileutils.WriteFile(env.TestFS, cmd.Args[4], []byte(testCase.data), fileperms.PrivateFile))
					}
				}

				return nil
			}
			report, err := executeReportTest(t, env)
			require.ErrorContains(t, err, "execution failure")
			assert.Equal(t, "incomplete", report.Status)
			invocation := report.Invocations[0]
			assert.Len(t, invocation.Reports, testCase.reports)
			assert.GreaterOrEqual(t, len(invocation.Errors), 2, "execution and collection failures must survive")
		})
	}
}

func TestComponentTestReportRetainsJUnitAfterLogFailure(t *testing.T) {
	for _, missing := range []bool{true, false} {
		name := "escaping nested log"
		if missing {
			name = "missing nested log"
		}

		t.Run(name, func(t *testing.T) {
			env := reportTestEnv(t)
			mockTMTCommands(t, env, func(native string) error {
				writeTMTFixture(t, env, native, "pass", "")

				if missing {
					require.NoError(t, env.TestFS.Remove(filepath.Join(native, "plans/all/execute/data/nested.out")))
				} else {
					path := filepath.Join(native, "plans/all/execute/results.yaml")
					data, err := fileutils.ReadFile(env.TestFS, path)
					require.NoError(t, err)
					require.NoError(t, fileutils.WriteFile(env.TestFS, path,
						[]byte(strings.ReplaceAll(string(data), "data/nested.out", "../../../../outside")),
						fileperms.PrivateFile))
				}

				return nil
			})

			report, err := executeReportTest(t, env)
			require.Error(t, err, "collection failure alone must fail the command")
			assert.Equal(t, testStatusIncomplete, report.Status)
			invocation := report.Invocations[0]
			assert.Equal(t, testStatusIncomplete, invocation.Status)
			require.Len(t, invocation.Reports, 1, "valid native results must still produce JUnit")
			require.NoError(t, validateTMTJUnit(env.TestFS,
				filepath.Join(report.ArtifactRoot, invocation.Reports[0].Path), 2))
			require.Len(t, invocation.Errors, 1)
			assert.Equal(t, "collection", invocation.Errors[0].Stage)

			if missing {
				assert.Contains(t, invocation.Errors[0].Message, "nested.out")
			} else {
				assert.Contains(t, invocation.Errors[0].Message, "not relative")
			}

			require.Len(t, invocation.Logs, 5, "omit only the unusable nested log")

			for _, log := range invocation.Logs {
				_, err := retainedFile(env.TestFS, report.ArtifactRoot, filepath.Join(report.ArtifactRoot, log.Path))
				require.NoError(t, err)
				assert.NotContains(t, log.Path, "nested.out")
				assert.NotContains(t, log.Path, "outside")
			}

			assert.NotEmpty(t, invocation.NativeArtifacts)
		})
	}
}

func TestComponentTestDryRunDoesNotCreateArtifacts(t *testing.T) {
	env := reportTestEnv(t)
	configureReportTestEnv(t, env, env.TestFS, true)
	env.CmdFactory.RunHandler = func(*exec.Cmd) error {
		t.Fatal("dry run must not invoke commands")

		return nil
	}
	report, err := executeReportTest(t, env)
	require.NoError(t, err)
	assert.Equal(t, "dry-run", report.Status)
	assert.Empty(t, report.ArtifactRoot)
	assert.Empty(t, report.Logs)
	assert.Equal(t, "dry-run", report.Invocations[0].Status)
	assert.Empty(t, report.Invocations[0].NativeArtifacts)

	_, err = env.TestFS.Stat("/work")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestComponentTestRetainsOtherPlansAfterCollectionFailure(t *testing.T) {
	env := reportTestEnv(t)
	mockTMTCommands(t, env, func(native string) error {
		writeTMTFixture(t, env, native, "pass", "")
		require.NoError(t, fileutils.WriteFile(env.TestFS, filepath.Join(native, "run.yaml"),
			[]byte("plans: [/plans/broken, /plans/all]\n"), fileperms.PrivateFile))
		require.NoError(t, fileutils.WriteFile(env.TestFS, filepath.Join(native, "plans/broken/execute/results.yaml"),
			[]byte("[malformed"), fileperms.PrivateFile))

		return nil
	})
	report, err := executeReportTest(t, env)
	require.Error(t, err)
	assert.Equal(t, "incomplete", report.Status)
	require.Len(t, report.Invocations[0].Reports, 1)
	assert.Equal(t, "/plans/all", report.Invocations[0].Reports[0].Plan)
	assert.NotEmpty(t, report.Invocations[0].NativeArtifacts)
}

type reportSymlinkFS struct {
	opctx.FS
	link string
}

type reportSymlinkInfo struct{ os.FileInfo }

func (reportSymlinkInfo) Mode() os.FileMode { return os.ModeSymlink | fileperms.PublicFile }

func (fs reportSymlinkFS) LstatIfPossible(path string) (os.FileInfo, bool, error) {
	info, err := fs.Stat(path)
	if err == nil && path == fs.link {
		return reportSymlinkInfo{info}, true, nil
	}

	return info, true, err
}

func TestRetainedFileBoundaries(t *testing.T) {
	env := testutils.NewTestEnv(t)
	require.NoError(t, fileutils.WriteFile(env.TestFS, "/owned/native/output.txt", []byte("log"), fileperms.PrivateFile))

	for _, link := range []string{"/owned", "/owned/native", "/owned/native/output.txt"} {
		_, err := retainedFile(reportSymlinkFS{FS: env.TestFS, link: link}, "/owned", "/owned/native/output.txt")
		require.ErrorContains(t, err, "symlink")
	}

	for _, path := range []string{"/outside", "/owned-other/output.txt", "/owned/../outside", "/owned/native", "/owned"} {
		_, err := retainedFile(env.TestFS, "/owned", path)
		require.Error(t, err, "path: %s", path)
	}

	rel, err := retainedFile(env.TestFS, "/owned", "/owned/native/output.txt")
	require.NoError(t, err)
	assert.Equal(t, "native/output.txt", rel)
}

func TestValidateTMTJUnitRejectsInvalidDocuments(t *testing.T) {
	env := testutils.NewTestEnv(t)
	for _, content := range []string{
		"", "<testsuites>", "<other/>", "<testsuites/><testsuites/>", "<testsuites/>tail",
		"<testsuites><testsuite><testcase/></testsuite></testsuites>",
	} {
		require.NoError(t, fileutils.WriteFile(env.TestFS, "/report.xml", []byte(content), fileperms.PrivateFile))
		require.Error(t, validateTMTJUnit(env.TestFS, "/report.xml", 0), "XML: %s", content)
	}
}

func TestComponentTestFreshArtifactsAndFailFast(t *testing.T) {
	env := reportTestEnv(t)
	component := env.Config.Components[testTMTComponentName]
	component.Tests = &projectconfig.ComponentTestsConfig{
		Tests: []projectconfig.TestRef{{Name: "first"}, {Name: "second"}},
	}

	env.Config.Components[testTMTComponentName] = component
	for _, name := range []string{"first", "second"} {
		env.Config.Tests[name] = projectconfig.TestDefinition{Type: "tmt", Tmt: map[string]any{}}
	}

	var runs []string

	mockTMTCommands(t, env, func(native string) error {
		runs = append(runs, native)
		writeTMTFixture(t, env, native, "fail", "")

		return errors.New("failed test")
	})
	first, err := executeReportTest(t, env)
	require.Error(t, err)
	second, err := executeReportTest(t, env)
	require.Error(t, err)
	assert.NotEqual(t, first.ArtifactRoot, second.ArtifactRoot)
	require.Len(t, runs, 2, "only the first selected invocation is attempted each time")

	for _, report := range []componentTestReport{first, second} {
		assert.Equal(t, "incomplete", report.Status)
		assert.Equal(t, "completed", report.Invocations[0].Status)
		assert.Equal(t, "not-run", report.Invocations[1].Status)
		assert.Empty(t, report.Invocations[1].Reports)
		assert.Empty(t, report.Invocations[1].Logs)
	}
}

type transcriptFailureFS struct {
	opctx.FS
	writeErr, closeErr error
	onlySuffix         string
}

func (fs transcriptFailureFS) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	file, err := fs.FS.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}

	if fs.onlySuffix != "" && !strings.HasSuffix(name, fs.onlySuffix) {
		return file, nil
	}

	return transcriptFailureFile{File: file, writeErr: fs.writeErr, closeErr: fs.closeErr}, nil
}

func TestComponentTestRunnerCaptureFailureIsIncomplete(t *testing.T) {
	env := reportTestEnv(t)
	configureReportTestEnv(t, env, transcriptFailureFS{
		FS: env.TestFS, closeErr: io.ErrClosedPipe, onlySuffix: "runner.log",
	}, false)
	mockTMTCommands(t, env, func(native string) error {
		writeTMTFixture(t, env, native, "pass", "")

		return nil
	})
	report, err := executeReportTest(t, env)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	assert.Equal(t, "incomplete", report.Status)
	assert.Equal(t, "incomplete", report.Invocations[0].Status)
	require.Len(t, report.Invocations[0].Reports, 1, "completed evidence must survive capture failure")
	require.Len(t, report.Invocations[0].Errors, 1, "capture failures must not be duplicated")
	assert.Equal(t, "collection", report.Invocations[0].Errors[0].Stage)
}

type transcriptFailureFile struct {
	afero.File
	writeErr, closeErr error
}

func (file transcriptFailureFile) Write(data []byte) (int, error) {
	if file.writeErr != nil {
		return 0, file.writeErr
	}

	return file.File.Write(data)
}

func (file transcriptFailureFile) Close() error { return errors.Join(file.File.Close(), file.closeErr) }

type reportFSFactory struct{ filesystem opctx.FS }

func (factory reportFSFactory) FS() opctx.FS { return factory.filesystem }

func TestComponentTestSetupAndCaptureFailures(t *testing.T) {
	for _, failure := range []string{"setup", "open", "write", "close"} {
		t.Run(failure, func(t *testing.T) {
			env := reportTestEnv(t)
			if failure == "setup" {
				env.CmdFactory.RunHandler = func(*exec.Cmd) error { return errors.New("setup failed") }
			} else {
				var filesystem opctx.FS

				switch failure {
				case "open":
					// A read-only filesystem also exercises artifact-root creation failure.
					filesystem = afero.NewReadOnlyFs(env.TestFS)
				case "write":
					filesystem = transcriptFailureFS{FS: env.TestFS, writeErr: io.ErrShortWrite}
				case "close":
					filesystem = transcriptFailureFS{FS: env.TestFS, closeErr: io.ErrClosedPipe}
				}

				configureReportTestEnv(t, env, filesystem, false)
			}

			report, err := executeReportTest(t, env)
			require.Error(t, err)
			assert.Equal(t, "incomplete", report.Status)
			require.Len(t, report.Errors, 1, "setup failures must not be duplicated")
			assert.Equal(t, "setup", report.Errors[0].Stage)
			assert.Equal(t, "not-run", report.Invocations[0].Status)
			assert.Empty(t, report.Invocations[0].Reports)
		})
	}
}
