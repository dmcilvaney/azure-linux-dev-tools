// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

//go:embed tmt_junit.py
var tmtJUnitExporter []byte

//go:embed tmt_junit.xml.j2
var tmtJUnitTemplate []byte

const tmtExecuteStage = "execute"

// Matches tmt.utils.sanitize_name in the pinned TMT version.
var tmtUnsafeName = regexp.MustCompile(`[^\p{L}\p{N}_/-]+`)

type tmtGuest struct {
	Name string `json:"name"           yaml:"name"`
	Role string `json:"role,omitempty" yaml:"role"`
}

type tmtResult struct {
	Name         string      `yaml:"name"`
	Result       string      `yaml:"result"`
	SerialNumber *int        `yaml:"serial-number"` //nolint:tagliatelle // Native TMT serialization uses kebab-case.
	Guest        *tmtGuest   `yaml:"guest"`
	Log          []string    `yaml:"log"`
	Check        []tmtResult `yaml:"check"`
	Subresult    []tmtResult `yaml:"subresult"`
}

// Reject symlinks, including intermediate directories: retained references must
// work after relocation and must never make the exporter read outside this run.
func retainedFile(fs opctx.FS, root string, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("artifact %#q escapes root %#q", path, root)
	}

	current := root
	for _, part := range append([]string{"."}, strings.Split(rel, string(filepath.Separator))...) {
		current = filepath.Join(current, part)

		var info os.FileInfo
		if lstater, ok := fs.(afero.Lstater); ok {
			info, _, err = lstater.LstatIfPossible(current)
		} else {
			info, err = fs.Stat(current)
		}

		if err != nil {
			return "", fmt.Errorf("inspect artifact %#q:\n%w", current, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("artifact %#q contains a symlink", path)
		}

		if current == path && !info.Mode().IsRegular() {
			return "", fmt.Errorf("artifact %#q is not a regular file", path)
		}
	}

	return filepath.ToSlash(rel), nil
}

func readTMTState(env *azldev.Env, root string, path string, value any) (string, error) {
	rel, err := retainedFile(env.FS(), root, path)
	if err != nil {
		return "", err
	}

	data, err := fileutils.ReadFile(env.FS(), path)
	if err != nil {
		return "", fmt.Errorf("read TMT state %#q:\n%w", path, err)
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return "", fmt.Errorf("empty TMT state %#q", path)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return "", fmt.Errorf("decode TMT state %#q:\n%w", path, err)
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("TMT state %#q must contain exactly one YAML document", path)
	}

	return rel, nil
}

func collectTMTArtifacts(
	env *azldev.Env, root string, settings tmtRunSettings, invocation *testInvocation,
) error {
	var failures []error

	problem := func(stage string, err error) {
		failures = append(failures, err)
		invocation.Errors = append(invocation.Errors, testReportError{Stage: stage, Message: err.Error()})
	}
	addOptionalLog := func(path string, kind string) {
		rel, err := retainedFile(env.FS(), root, path)
		if err == nil {
			invocation.Logs = append(invocation.Logs, testLog{Kind: kind, Path: rel})
		} else if !errors.Is(err, os.ErrNotExist) {
			problem("collection", err)
		}
	}
	addOptionalLog(filepath.Join(settings.WorkDir, "runner.log"), "runner")
	nativeDir := filepath.Join(settings.WorkDir, "native")
	addOptionalLog(filepath.Join(nativeDir, "log.txt"), "runner")

	var run struct {
		Plans []string `yaml:"plans"`
	}

	rel, err := readTMTState(env, root, filepath.Join(nativeDir, "run.yaml"), &run)
	if err != nil {
		problem("collection", err)

		return errors.Join(failures...)
	}

	invocation.NativeArtifacts = append(invocation.NativeArtifacts, testArtifact{Format: "tmt-run", Path: rel})

	if len(run.Plans) == 0 {
		problem("collection", errors.New("TMT run contains no plans"))
	}

	slices.Sort(run.Plans)
	run.Plans = slices.Compact(run.Plans)

	directoryCounts := make(map[string]int, len(run.Plans))
	for _, plan := range run.Plans {
		directoryCounts[tmtPlanDirectory(plan)]++
	}

	for index, plan := range run.Plans {
		if !strings.HasPrefix(plan, "/") || !filepath.IsLocal(strings.TrimPrefix(plan, "/")) {
			problem("collection", fmt.Errorf("invalid native TMT plan name %#q", plan))

			continue
		}

		directory := tmtPlanDirectory(plan)
		if !filepath.IsLocal(directory) || directoryCounts[directory] != 1 {
			problem("collection", fmt.Errorf("TMT plan %#q has an invalid or shared native directory %#q", plan, directory))

			continue
		}

		planDir := filepath.Join(nativeDir, directory)
		collectTMTPlan(env, root, settings, invocation, plan, planDir, index, problem)
	}

	slices.SortFunc(invocation.Logs, func(a, b testLog) int { return strings.Compare(a.Path, b.Path) })
	invocation.Logs = slices.CompactFunc(invocation.Logs, func(a, b testLog) bool { return a.Path == b.Path })
	slices.SortFunc(invocation.NativeArtifacts, func(a, b testArtifact) int { return strings.Compare(a.Path, b.Path) })

	if len(failures) == 0 {
		invocation.Status = testStatusCompleted
	}

	return errors.Join(failures...)
}

func tmtPlanDirectory(plan string) string {
	return strings.TrimLeft(strings.Trim(tmtUnsafeName.ReplaceAllString(plan, "-"), "-"), "/")
}

func collectTMTPlan(
	env *azldev.Env, root string, settings tmtRunSettings, invocation *testInvocation,
	plan string, planDir string, index int, problem func(string, error),
) {
	for _, stage := range []string{"discover", "provision", "prepare", tmtExecuteStage, "finish", "report", "cleanup"} {
		var step struct {
			Status string `yaml:"status"`
		}

		stepDir := filepath.Join(planDir, stage)

		rel, err := readTMTState(env, root, filepath.Join(stepDir, "step.yaml"), &step)
		if err != nil {
			problem(stage, err)
		} else {
			invocation.NativeArtifacts = append(invocation.NativeArtifacts, testArtifact{
				Format: "tmt-step", Plan: plan, Path: rel,
			})
			if step.Status != "done" {
				problem(stage, fmt.Errorf("TMT plan %#q %s status is %#q, not done", plan, stage, step.Status))
			}
		}

		resultPath := filepath.Join(stepDir, "results.yaml")

		var results []tmtResult

		rel, err = readTMTState(env, root, resultPath, &results)
		if err != nil {
			if stage == tmtExecuteStage || !errors.Is(err, os.ErrNotExist) {
				problem("collection", err)
			}

			continue
		}

		invocation.NativeArtifacts = append(invocation.NativeArtifacts, testArtifact{
			Format: "tmt-results", Plan: plan, Path: rel,
		})

		logErr := collectTMTResultLogs(env.FS(), root, stepDir, results, testLog{Plan: plan, Kind: stage}, invocation)
		if logErr != nil {
			problem("collection", logErr)
		}

		if stage != tmtExecuteStage {
			continue
		}

		if err := validateTMTResults(results, problem); err != nil {
			problem("collection", fmt.Errorf("TMT plan %#q:\n%w", plan, err))

			continue
		}

		if err := exportTMTJUnit(env, root, settings, invocation, plan, resultPath, index, len(results)); err != nil {
			problem("report", err)
		}
	}
}

func validateTMTResults(results []tmtResult, problem func(string, error)) error {
	if len(results) == 0 {
		return errors.New("no persisted test results")
	}

	var invalid []error

	for _, result := range results {
		if !slices.Contains([]string{"pass", "fail", "skip", "info", "warn", "error", "pending"}, result.Result) {
			invalid = append(invalid, fmt.Errorf("invalid TMT result %#q for %#q", result.Result, result.Name))
		}

		if result.Result == "pending" {
			problem(tmtExecuteStage, fmt.Errorf("TMT test %#q is pending", result.Name))
		}
	}

	return errors.Join(invalid...)
}

func collectTMTResultLogs(
	fs opctx.FS, root string, stepDir string, results []tmtResult, association testLog, invocation *testInvocation,
) error {
	failures := make([]error, 0, len(results))

	for _, result := range results {
		log := association
		if result.Guest != nil {
			log.Guest = result.Guest
		}

		if result.SerialNumber != nil {
			log.SerialNumber = result.SerialNumber
		}

		if association.Kind == tmtExecuteStage {
			log.Kind, log.Test = "test", result.Name
		}

		for _, path := range result.Log {
			if !filepath.IsLocal(path) {
				failures = append(failures, fmt.Errorf("TMT log %#q is not relative to its step", path))

				continue
			}

			rel, err := retainedFile(fs, root, filepath.Join(stepDir, path))
			if err != nil {
				failures = append(failures, err)

				continue
			}

			log.Path = rel
			invocation.Logs = append(invocation.Logs, log)
		}

		log.Kind = "check"
		failures = append(failures, collectTMTResultLogs(fs, root, stepDir, result.Check, log, invocation))
		log.Kind = "subresult"
		failures = append(failures, collectTMTResultLogs(fs, root, stepDir, result.Subresult, log, invocation))
	}

	return errors.Join(failures...)
}

func exportTMTJUnit(
	env *azldev.Env, root string, settings tmtRunSettings, invocation *testInvocation,
	plan string, resultPath string, index int, count int,
) error {
	script := filepath.Join(root, "export-junit.py")
	template := filepath.Join(root, "junit.xml.j2")

	if err := fileutils.WriteFile(env.FS(), script, tmtJUnitExporter, fileperms.PrivateFile); err != nil {
		return fmt.Errorf("write native JUnit exporter:\n%w", err)
	}

	if err := fileutils.WriteFile(env.FS(), template, tmtJUnitTemplate, fileperms.PrivateFile); err != nil {
		return fmt.Errorf("write JUnit template:\n%w", err)
	}

	reportPath := filepath.Join(settings.WorkDir, fmt.Sprintf("junit-%03d.xml", index+1))

	python := filepath.Join(filepath.Dir(settings.TMTProgramPath), tmtPythonProgram)
	if err := runHostCommand(
		env, settings.SetupLog, "", python, script, resultPath, plan, reportPath, template,
	); err != nil {
		return err
	}

	rel, err := retainedFile(env.FS(), root, reportPath)
	if err != nil {
		return err
	}

	if err := validateTMTJUnit(env.FS(), reportPath, count); err != nil {
		return err
	}

	invocation.Reports = append(invocation.Reports, testArtifact{Format: "junit", Plan: plan, Path: rel})

	return nil
}

func validateTMTJUnit(fs opctx.FS, path string, expectedCount int) (err error) {
	file, err := fs.Open(path)
	if err != nil {
		return fmt.Errorf("open JUnit %#q:\n%w", path, err)
	}

	defer func() { err = errors.Join(err, file.Close()) }()

	decoder := xml.NewDecoder(file)
	depth, count, roots := 0, 0, 0

	for {
		token, readErr := decoder.Token()
		if errors.Is(readErr, io.EOF) {
			break
		}

		if readErr != nil {
			return fmt.Errorf("invalid JUnit %#q:\n%w", path, readErr)
		}

		switch element := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++

				if element.Name.Local != "testsuites" {
					return fmt.Errorf("unexpected JUnit root %#q", element.Name.Local)
				}
			}

			if element.Name.Local == "testcase" {
				count++
			}

			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(element)) != "" {
				return fmt.Errorf("unexpected content outside JUnit root in %#q", path)
			}
		}
	}

	if roots != 1 || count != expectedCount {
		return fmt.Errorf("JUnit %#q has %d roots and %d testcases; expected one root and %d testcases",
			path, roots, count, expectedCount)
	}

	return nil
}
