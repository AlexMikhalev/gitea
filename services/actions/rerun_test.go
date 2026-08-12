// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"testing"

	actions_model "code.gitea.io/gitea/models/actions"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	"code.gitea.io/gitea/modules/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAllRerunJobs(t *testing.T) {
	job1 := &actions_model.ActionRunJob{JobID: "job1"}
	job2 := &actions_model.ActionRunJob{JobID: "job2", Needs: []string{"job1"}}
	job3 := &actions_model.ActionRunJob{JobID: "job3", Needs: []string{"job2"}}
	job4 := &actions_model.ActionRunJob{JobID: "job4", Needs: []string{"job2", "job3"}}

	jobs := []*actions_model.ActionRunJob{job1, job2, job3, job4}

	testCases := []struct {
		job       *actions_model.ActionRunJob
		rerunJobs []*actions_model.ActionRunJob
	}{
		{
			job1,
			[]*actions_model.ActionRunJob{job1, job2, job3, job4},
		},
		{
			job2,
			[]*actions_model.ActionRunJob{job2, job3, job4},
		},
		{
			job3,
			[]*actions_model.ActionRunJob{job3, job4},
		},
		{
			job4,
			[]*actions_model.ActionRunJob{job4},
		},
	}

	for _, tc := range testCases {
		rerunJobs := GetAllRerunJobs(tc.job, jobs)
		assert.ElementsMatch(t, tc.rerunJobs, rerunJobs)
	}
}

func TestGetFailedRerunJobs(t *testing.T) {
	// IDs must be non-zero to distinguish jobs in the dedup set.
	makeJob := func(id int64, jobID string, status actions_model.Status, needs ...string) *actions_model.ActionRunJob {
		return &actions_model.ActionRunJob{ID: id, JobID: jobID, Status: status, Needs: needs}
	}

	t.Run("no failed jobs returns empty", func(t *testing.T) {
		jobs := []*actions_model.ActionRunJob{
			makeJob(1, "job1", actions_model.StatusSuccess),
			makeJob(2, "job2", actions_model.StatusSkipped, "job1"),
		}
		assert.Empty(t, GetFailedRerunJobs(jobs))
	})

	t.Run("single failed job with no dependents", func(t *testing.T) {
		job1 := makeJob(1, "job1", actions_model.StatusFailure)
		job2 := makeJob(2, "job2", actions_model.StatusSuccess)
		jobs := []*actions_model.ActionRunJob{job1, job2}

		result := GetFailedRerunJobs(jobs)
		assert.ElementsMatch(t, []*actions_model.ActionRunJob{job1}, result)
	})

	t.Run("failed job pulls in downstream dependents", func(t *testing.T) {
		// job1 failed; job2 depends on job1 (skipped); job3 depends on job2 (skipped)
		job1 := makeJob(1, "job1", actions_model.StatusFailure)
		job2 := makeJob(2, "job2", actions_model.StatusSkipped, "job1")
		job3 := makeJob(3, "job3", actions_model.StatusSkipped, "job2")
		job4 := makeJob(4, "job4", actions_model.StatusSuccess) // unrelated, must not appear
		jobs := []*actions_model.ActionRunJob{job1, job2, job3, job4}

		result := GetFailedRerunJobs(jobs)
		assert.ElementsMatch(t, []*actions_model.ActionRunJob{job1, job2, job3}, result)
	})

	t.Run("multiple independent failed jobs each pull in their own dependents", func(t *testing.T) {
		// job1 failed -> job3 depends on job1
		// job2 failed -> job4 depends on job2
		job1 := makeJob(1, "job1", actions_model.StatusFailure)
		job2 := makeJob(2, "job2", actions_model.StatusFailure)
		job3 := makeJob(3, "job3", actions_model.StatusSkipped, "job1")
		job4 := makeJob(4, "job4", actions_model.StatusSkipped, "job2")
		jobs := []*actions_model.ActionRunJob{job1, job2, job3, job4}

		result := GetFailedRerunJobs(jobs)
		assert.ElementsMatch(t, []*actions_model.ActionRunJob{job1, job2, job3, job4}, result)
	})

	t.Run("shared downstream dependent is not duplicated", func(t *testing.T) {
		// job1 and job2 both failed; job3 depends on both
		job1 := makeJob(1, "job1", actions_model.StatusFailure)
		job2 := makeJob(2, "job2", actions_model.StatusFailure)
		job3 := makeJob(3, "job3", actions_model.StatusSkipped, "job1", "job2")
		jobs := []*actions_model.ActionRunJob{job1, job2, job3}

		result := GetFailedRerunJobs(jobs)
		assert.ElementsMatch(t, []*actions_model.ActionRunJob{job1, job2, job3}, result)
		assert.Len(t, result, 3) // job3 must appear exactly once
	})

	t.Run("successful downstream job of a failed job is still included", func(t *testing.T) {
		// job1 failed; job2 succeeded but depends on job1 — downstream is always rerun
		// regardless of its own status (GetAllRerunJobs includes all transitive dependents)
		job1 := makeJob(1, "job1", actions_model.StatusFailure)
		job2 := makeJob(2, "job2", actions_model.StatusSuccess, "job1")
		jobs := []*actions_model.ActionRunJob{job1, job2}

		result := GetFailedRerunJobs(jobs)
		assert.ElementsMatch(t, []*actions_model.ActionRunJob{job1, job2}, result)
	})
}

func TestRerunValidation(t *testing.T) {
	runningRun := &actions_model.ActionRun{Status: actions_model.StatusRunning}

	t.Run("RerunWorkflowRunJobs rejects a non-done run", func(t *testing.T) {
		jobs := []*actions_model.ActionRunJob{
			{ID: 1, JobID: "job1"},
		}
		err := RerunWorkflowRunJobs(context.Background(), nil, runningRun, jobs)
		require.Error(t, err)
		assert.ErrorIs(t, err, util.ErrInvalidArgument)
	})

	t.Run("RerunWorkflowRunJobs rejects a non-done run when failed jobs exist", func(t *testing.T) {
		jobs := []*actions_model.ActionRunJob{
			{ID: 1, JobID: "job1", Status: actions_model.StatusFailure},
		}
		err := RerunWorkflowRunJobs(context.Background(), nil, runningRun, GetFailedRerunJobs(jobs))
		require.Error(t, err)
		assert.ErrorIs(t, err, util.ErrInvalidArgument)
	})
}

// TestRerunValidationWithNoJobs covers a fork-side divergence from upstream #36924.
//
// Upstream returns nil as soon as the job set is empty, before any validation. A
// rerun-failed request therefore reported success without checking anything whenever
// nothing had failed — including on a still-running run or a disabled workflow. The
// jobs are validated first here, and an empty set is an explicit error rather than a
// silent success.
//
// The run must not be mutated in either case: prepareRunRerun resets a run to Waiting,
// so validating by calling it would leave a run with nothing to schedule waiting forever.
func TestRerunValidationWithNoJobs(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

	// A run whose jobs all succeeded: GetFailedRerunJobs yields nothing.
	succeededJobs := []*actions_model.ActionRunJob{
		{ID: 1, JobID: "job1", Status: actions_model.StatusSuccess},
		{ID: 2, JobID: "job2", Status: actions_model.StatusSuccess, Needs: []string{"job1"}},
	}
	require.Empty(t, GetFailedRerunJobs(succeededJobs), "precondition: no failed jobs")

	t.Run("still-running run with zero failed jobs is rejected", func(t *testing.T) {
		run := &actions_model.ActionRun{
			ID: 1, WorkflowID: "test.yaml",
			Status: actions_model.StatusRunning, Started: 100, Stopped: 200,
		}

		err := RerunWorkflowRunJobs(t.Context(), repo, run, GetFailedRerunJobs(succeededJobs))

		require.Error(t, err, "must not report success for a rerun it never validated")
		assert.ErrorIs(t, err, util.ErrInvalidArgument)
		assert.ErrorContains(t, err, "not done")
		assert.NotErrorIs(t, err, ErrNoJobsToRerun, "the run state is the reason, not the empty set")
		assert.Equal(t, actions_model.StatusRunning, run.Status, "run must not be reset")
	})

	t.Run("completed run with zero failed jobs reports nothing to rerun", func(t *testing.T) {
		run := &actions_model.ActionRun{
			ID: 1, WorkflowID: "test.yaml",
			Status: actions_model.StatusSuccess, Started: 100, Stopped: 200,
		}

		err := RerunWorkflowRunJobs(t.Context(), repo, run, GetFailedRerunJobs(succeededJobs))

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNoJobsToRerun)
		assert.Equal(t, actions_model.StatusSuccess, run.Status, "run must not be reset")
		assert.EqualValues(t, 100, run.Started, "timestamps must not be zeroed")
		assert.EqualValues(t, 200, run.Stopped)
	})

	t.Run("ErrNoJobsToRerun reaches the API's 400 mapping", func(t *testing.T) {
		// handleWorkflowRerunError maps util.ErrInvalidArgument to 400 Bad Request,
		// so this is what stops the endpoint answering 201 Created.
		assert.ErrorIs(t, ErrNoJobsToRerun, util.ErrInvalidArgument)
	})
}
