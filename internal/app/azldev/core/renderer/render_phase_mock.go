// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"log/slog"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/sources"
)

// batchMockProcess runs rpmautospec and spectool for all prepared components in
// a single mock chroot invocation. Returns a map from component name to result.
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
	prepared []*preparedComponent,
) map[string]*sources.ComponentMockResult {
	if len(prepared) == 0 {
		return nil
	}

	// Build batch inputs from prepared components.
	inputs := make([]sources.ComponentInput, len(prepared))
	for idx, prep := range prepared {
		inputs[idx] = sources.ComponentInput{
			Name:         prep.comp.GetName(),
			SpecFilename: prep.specFilename,
		}
	}

	mockResults, err := mockProcessor.BatchProcess(env, env, stagingDir, inputs, env.FS(), env.CPUBoundConcurrency())
	if err != nil {
		slog.Error("Batch mock processing failed", "error", err)
		// Return empty map — all components will get reported as errors in phase 3.
		return nil
	}

	// Build lookup map for phase 3.
	resultMap := make(map[string]*sources.ComponentMockResult, len(mockResults))
	for idx := range mockResults {
		resultMap[mockResults[idx].Name] = &mockResults[idx]
	}

	return resultMap
}
