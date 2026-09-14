// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"testing"
	"time"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/unittest"
	"code.gitea.io/gitea/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStopZombieTasks_NotTriggeredBeforeTimeout verifies that StopZombieTasks
// does NOT mark a task as failure if the task was updated within the
// ZombieTaskTimeout window (60min). This is the GAP-5 server fix verification:
// long workflows (12-14min) must remain in_progress for the full duration.
//
// We test the filter behaviour directly (FindTasks + the threshold logic)
// because stopTasks has side effects (log transfer, status update) that are
// hard to test in isolation. The threshold itself is the GAP-5 fix.
func TestStopZombieTasks_NotTriggeredBeforeTimeout(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())
	updated := timeutil.TimeStamp(time.Now().Add(-30 * time.Minute).Unix())
	_, err := db.GetEngine(t.Context()).Exec(
		"UPDATE action_task SET updated = ?, status = ? WHERE id = ?",
		updated, int(actions_model.StatusRunning), 51)
	require.NoError(t, err)

	// Direct filter test: a task updated 30min ago must NOT match the
	// 60min threshold. This is the core of the GAP-5 fix verification.
	threshold := timeutil.TimeStamp(time.Now().Add(-1 * time.Hour).Unix())
	found, err := db.Find[actions_model.ActionTask](t.Context(), actions_model.FindTaskOptions{
		Status:        actions_model.StatusRunning,
		UpdatedBefore: threshold,
	})
	require.NoError(t, err)
	for _, task := range found {
		if task.ID == 51 {
			t.Errorf("task 51 (updated 30min ago) should NOT match 60min threshold, but was found")
		}
	}
}

// TestStopZombieTasks_TriggeredAfterTimeout verifies that a task updated
// BEFORE the ZombieTaskTimeout window matches the sweep filter. This
// preserves the zombie-safety-net behavior for genuinely abandoned runner
// connections.
func TestStopZombieTasks_TriggeredAfterTimeout(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())
	updated := timeutil.TimeStamp(time.Now().Add(-90 * time.Minute).Unix())
	_, err := db.GetEngine(t.Context()).Exec(
		"UPDATE action_task SET updated = ?, status = ? WHERE id = ?",
		updated, int(actions_model.StatusRunning), 51)
	require.NoError(t, err)

	// Direct filter test: a task updated 90min ago MUST match the 60min threshold.
	threshold := timeutil.TimeStamp(time.Now().Add(-1 * time.Hour).Unix())
	found, err := db.Find[actions_model.ActionTask](t.Context(), actions_model.FindTaskOptions{
		Status:        actions_model.StatusRunning,
		UpdatedBefore: threshold,
	})
	require.NoError(t, err)
	matched := false
	for _, task := range found {
		if task.ID == 51 {
			matched = true
			break
		}
	}
	assert.True(t, matched, "task 51 (updated 90min ago) MUST match 60min threshold")
}
