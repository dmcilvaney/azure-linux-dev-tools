// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package renderer

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/parmap"
)

// parallelPrepare runs phase 1 (clone + overlay + synthetic git) over every
// job concurrently, bounded by [azldev.Env.IOBoundConcurrency].
//
// Inputs are [NewJob]s; outputs are the same jobs advanced to [PreparedJob].
// Failure and cancellation are recorded inside each job; the type-system
// view stays uniform across all three outcomes so the orchestrator can keep
// iterating without branching.
func parallelPrepare(env *azldev.Env, newJobs []NewJob) []PreparedJob {
	progressEvent := env.StartEvent("Preparing component sources", "count", len(newJobs))
	defer progressEvent.End()

	workerEnv, cancel := env.WithCancel()
	defer cancel()

	total := int64(len(newJobs))

	parmapResults := parmap.Map(
		workerEnv,
		env.IOBoundConcurrency(),
		newJobs,
		func(done, _ int) { progressEvent.SetProgress(int64(done), total) },
		func(_ context.Context, j NewJob) PreparedJob {
			// workerEnv (captured) is the effective context for this call chain;
			// the parmap-supplied ctx is identical and unused here.
			return j.Prepare(workerEnv)
		},
	)

	preparedJobs := make([]PreparedJob, len(newJobs))

	for idx, result := range parmapResults {
		if result.Cancelled {
			// Worker never started — ctx ended before parmap reached it.
			preparedJobs[idx] = newJobs[idx].MarkCancelled()
		} else {
			preparedJobs[idx] = result.Value
		}
	}

	return preparedJobs
}

// findSpecFile locates the spec file for a component in the given directory.
// Returns the full path; callers that want just the basename should call
// [filepath.Base] on the return value.
func findSpecFile(fs opctx.FS, dir, componentName string) (string, error) {
	specPath := filepath.Join(dir, componentName+".spec")

	exists, err := fileutils.Exists(fs, specPath)
	if err != nil {
		return "", fmt.Errorf("checking spec file %#q:\n%w", specPath, err)
	}

	if !exists {
		return "", fmt.Errorf("expected spec file %#q not found for component %#q", specPath, componentName)
	}

	return specPath, nil
}
