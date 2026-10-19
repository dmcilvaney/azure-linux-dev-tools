// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build e2e && linux

package scenario_tests

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/scenario/internal/cmdtest"
	"github.com/microsoft/azure-linux-dev-tools/scenario/internal/containertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tmtManifestFile struct {
	Path   string `json:"path"`
	Plan   string `json:"plan"`
	Kind   string `json:"kind"`
	Format string `json:"format"`
}

type tmtManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	ArtifactRoot  string            `json:"artifactRoot"`
	Status        string            `json:"status"`
	Logs          []tmtManifestFile `json:"logs"`
	Invocations   []struct {
		Status          string            `json:"status"`
		ExitCode        *int              `json:"exitCode"`
		Reports         []tmtManifestFile `json:"reports"`
		Logs            []tmtManifestFile `json:"logs"`
		NativeArtifacts []tmtManifestFile `json:"nativeArtifacts"`
		Errors          []struct {
			Stage   string `json:"stage"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"invocations"`
}

func TestTMTLocalReporting(t *testing.T) {
	if testing.Short() || os.Getenv("TESTING_ENABLE_TMT_LOCAL_E2E") == "" {
		t.Skip("opt in with 'mage e2e -target=tmt-local'")
	}
	base, err := filepath.Abs(filepath.Join("..", "out", "tmt-local-e2e"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(base, fileperms.PrivateDir))
	artifacts, err := os.MkdirTemp(base, "run-")
	require.NoError(t, err)
	t.Logf("Retained local TMT artifacts: %s", artifacts)

	test := cmdtest.NewScenarioTest("fixture/run.sh").
		WithCustomCmd("bash").
		WithTimeout(20 * time.Minute).
		InContainer().
		WithDockerfile("Dockerfile.tmt").
		WithNetwork().
		WithExtraMounts([]containertest.ContainerMount{
			containertest.NewContainerMount(artifacts, "/artifacts", nil),
		})
	test.AddDirRecursive(t, "fixture", filepath.Join("testdata", "tmt-local"))
	test.AddSingleFile("project/azldev.toml", filepath.Join("testdata", "tmt-virtual", "azldev.toml"))
	test.AddSingleFile("project/azldev-tmt-probe.spec", filepath.Join("testdata", "tmt-virtual", "azldev-tmt-probe.spec"))
	results, err := test.Run(t)
	require.NoError(t, os.WriteFile(filepath.Join(artifacts, "container.stdout"), []byte(results.Stdout), fileperms.PrivateFile))
	require.NoError(t, os.WriteFile(filepath.Join(artifacts, "container.stderr"), []byte(results.Stderr), fileperms.PrivateFile))
	require.NoError(t, err)
	results.AssertZeroExitCode(t)

	roots := map[string]bool{}
	for _, tc := range []struct {
		name, status                 string
		azldevExit, tmtExit, reports int
	}{
		{"pass", "completed", 0, 0, 1},
		{"mixed", "completed", 1, 2, 1},
		{"skip", "completed", 1, 4, 1},
		{"fail", "completed", 1, 1, 1},
		{"warn", "completed", 1, 1, 1},
		{"with.dots", "completed", 0, 0, 1},
		{"empty", "incomplete", 1, 3, 0},
		{"prepare-failure", "incomplete", 1, 2, 1},
		{"finish-failure", "incomplete", 1, 2, 1},
		{"report-failure", "incomplete", 1, 2, 1},
		{"multi", "completed", 0, 0, 2},
		{"interrupted", "incomplete", 1, -1, 1},
		{"cancelled-sigint", "incomplete", 1, -1, 0},
		{"cancelled-sigterm", "incomplete", 1, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(artifacts, tc.name+".json"))
			require.NoError(t, err)
			var manifest tmtManifest
			decoder := json.NewDecoder(bytes.NewReader(data))
			require.NoError(t, decoder.Decode(&manifest), "stdout: %s", data)
			require.ErrorIs(t, decoder.Decode(new(any)), io.EOF)
			exit, err := os.ReadFile(filepath.Join(artifacts, tc.name+".exit"))
			require.NoError(t, err)
			assert.Equal(t, strconv.Itoa(tc.azldevExit)+"\n", string(exit))
			assert.Equal(t, 1, manifest.SchemaVersion)
			assert.Equal(t, tc.status, manifest.Status, "%s", data)
			require.Len(t, manifest.Invocations, 1)
			invocation := manifest.Invocations[0]
			assert.Equal(t, tc.status, invocation.Status)
			if tc.tmtExit < 0 {
				assert.Nil(t, invocation.ExitCode, "a signal is not an ordinary exit code")
			} else {
				require.NotNil(t, invocation.ExitCode)
				assert.Equal(t, tc.tmtExit, *invocation.ExitCode)
			}
			assert.Len(t, invocation.Reports, tc.reports, "%s", data)
			assert.False(t, roots[manifest.ArtifactRoot], "work-directory reuse must not reuse a run")
			roots[manifest.ArtifactRoot] = true
			rel, err := filepath.Rel("/artifacts", manifest.ArtifactRoot)
			require.NoError(t, err)
			require.True(t, filepath.IsLocal(rel))
			root := filepath.Join(artifacts, rel)
			allFiles := append([]tmtManifestFile{}, manifest.Logs...)
			allFiles = append(allFiles, invocation.Logs...)
			allFiles = append(allFiles, invocation.NativeArtifacts...)
			allFiles = append(allFiles, invocation.Reports...)
			for _, file := range allFiles {
				require.True(t, filepath.IsLocal(file.Path), "path: %s", file.Path)
				info, statErr := os.Stat(filepath.Join(root, file.Path))
				require.NoError(t, statErr)
				require.True(t, info.Mode().IsRegular())
			}
			for _, report := range invocation.Reports {
				validateLocalTMTJUnit(t, filepath.Join(root, report.Path), tc.name)
			}
			if strings.HasPrefix(tc.name, "cancelled-") {
				assert.NotEmpty(t, invocation.Errors)
				var exportCancelled, retainedResults, retainedTestLog, retainedRunnerLog bool
				for _, problem := range invocation.Errors {
					if problem.Stage == "report" && strings.Contains(problem.Message, "context canceled") {
						exportCancelled = true
					}
				}
				for _, file := range invocation.NativeArtifacts {
					if file.Format == "tmt-results" && strings.Contains(file.Path, "/execute/") {
						data, readErr := os.ReadFile(filepath.Join(root, file.Path))
						require.NoError(t, readErr)
						assert.Contains(t, string(data), "result: pass")
						retainedResults = true
					}
				}
				for _, file := range invocation.Logs {
					data, readErr := os.ReadFile(filepath.Join(root, file.Path))
					require.NoError(t, readErr)
					if file.Kind == "test" && strings.Contains(string(data), "noisy-probe-stdout") {
						retainedTestLog = true
					}
					if file.Kind == "runner" && strings.Contains(string(data), "interrupted-runner-stderr") {
						retainedRunnerLog = true
					}
				}
				assert.True(t, exportCancelled, "cancelled exporter must be reported")
				assert.True(t, retainedResults, "persisted native results must survive cancellation")
				assert.True(t, retainedTestLog, "persisted test logs must remain referenced")
				assert.True(t, retainedRunnerLog, "runner transcript must survive cancellation")
			}
			if tc.name == "pass" {
				assertLocalTMTLogsAndRelocation(t, root, allFiles)
				var nativeReport string
				for _, file := range invocation.NativeArtifacts {
					if strings.HasSuffix(file.Path, "/report/step.yaml") {
						nativeReport = file.Path
					}
				}
				require.NotEmpty(t, nativeReport)
				state, readErr := os.ReadFile(filepath.Join(root, nativeReport))
				require.NoError(t, readErr)
				assert.Contains(t, string(state), "supplied-junit", "existing reporting must be preserved")
			}
		})
	}
	validateLocalTMTJUnit(t, filepath.Join(artifacts, "projection.xml"), "projection")
}

func validateLocalTMTJUnit(t *testing.T, path string, caseName string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var document struct {
		Suites []struct {
			Name  string `xml:"name,attr"`
			Cases []struct {
				Name    string    `xml:"name,attr"`
				Class   string    `xml:"classname,attr"`
				Failure *struct{} `xml:"failure"`
				Error   *struct{} `xml:"error"`
				Skipped *struct{} `xml:"skipped"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	require.NoError(t, xml.Unmarshal(data, &document))
	require.Len(t, document.Suites, 1)
	counts := map[string]int{}
	identities := map[string]bool{}
	for _, result := range document.Suites[0].Cases {
		assert.Contains(t, result.Class, "/plans/")
		assert.Contains(t, result.Name, " #", "serial/guest identity must survive publication")
		identity := result.Class + "\x00" + result.Name
		assert.False(t, identities[identity], "duplicate testcase identity: %s", result.Name)
		identities[identity] = true
		switch {
		case result.Failure != nil:
			counts["fail"]++
		case result.Error != nil:
			counts["error"]++
		case result.Skipped != nil:
			counts["skip"]++
		default:
			counts["pass"]++
		}
	}
	var expected map[string]int
	switch caseName {
	case "pass", "with.dots", "finish-failure", "report-failure", "interrupted":
		expected = map[string]int{"pass": 1}
	case "fail":
		expected = map[string]int{"fail": 1}
	case "skip":
		expected = map[string]int{"skip": 1}
	case "warn", "prepare-failure":
		expected = map[string]int{"error": 1}
	case "mixed":
		expected = map[string]int{"pass": 1, "fail": 1, "skip": 2, "error": 2}
	case "projection":
		expected = map[string]int{"pass": 1, "fail": 1, "skip": 2, "error": 3}
	case "multi":
		if document.Suites[0].Name == "/plans/skip" {
			expected = map[string]int{"skip": 1}
		} else {
			require.Equal(t, "/plans/pass", document.Suites[0].Name)
			expected = map[string]int{"pass": 1}
		}
	default:
		t.Fatalf("missing JUnit expectations for %s", caseName)
	}
	assert.Equal(t, expected, counts, "completed testcase outcomes must survive later lifecycle failures")
}

func assertLocalTMTLogsAndRelocation(t *testing.T, root string, files []tmtManifestFile) {
	t.Helper()
	relocated := t.TempDir()
	require.NoError(t, os.CopyFS(relocated, os.DirFS(root)))
	var transcript strings.Builder
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(root, file.Path))
		require.NoError(t, err)
		transcript.Write(data)
		copied, err := os.ReadFile(filepath.Join(relocated, file.Path))
		require.NoError(t, err)
		assert.Equal(t, data, copied)
	}
	assert.Contains(t, transcript.String(), "noisy-probe-stdout")
	assert.Contains(t, transcript.String(), "noisy-probe-stderr")
}
