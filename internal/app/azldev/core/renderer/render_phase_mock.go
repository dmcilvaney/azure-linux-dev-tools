// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"log/slog"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
)

// batchMockProcess runs rpmautospec and spectool for all viable prepared
// jobs in a single mock chroot invocation. Returns a map from component
// name to result.
//
// Jobs that failed phase 1 or were cancelled are excluded from the batch
// (their SpecFilename() returns ""); they'll surface in
// [PreparedJob.ApplyMockResult] as a nil mockResult lookup, which the job
// already handles as a passthrough since its err is set.
//
// NOTE: All components share one mock chroot initialized from the project's
// default distro. Phase 1 resolves distro per-component for source fetching,
// but the mock environment (macros, rpmautospec version, etc.) is uniform.
// This matches the current Koji build model where all components target the
// same distro version.
func batchMockProcess(
	env *azldev.Env,
	mockProcessor *sources.MockProcessor,
	stagingDir string,
	prepared []PreparedJob,
) map[string]*sources.ComponentMockResult {
	// Build batch inputs from viable jobs (skip failed/cancelled).
	inputs := make([]sources.ComponentInput, 0, len(prepared))

	for _, job := range prepared {
		if job.SpecFilename() == "" {
			continue
		}

		inputs = append(inputs, sources.ComponentInput{
			Name:         job.ComponentName(),
			SpecFilename: job.SpecFilename(),
		})
	}

	if len(inputs) == 0 {
		return nil
	}

	mockResults, err := mockProcessor.BatchProcess(env, env, stagingDir, inputs, env.FS(), env.CPUBoundConcurrency())
	if err != nil {
		slog.Error("Batch mock processing failed", "error", err)
		// Return empty map — every consuming job will see a nil lookup and
		// surface "no mock result" as its own error.
		return nil
	}

	resultMap := make(map[string]*sources.ComponentMockResult, len(mockResults))
	for idx := range mockResults {
		resultMap[mockResults[idx].Name] = &mockResults[idx]
	}

	return resultMap
}
