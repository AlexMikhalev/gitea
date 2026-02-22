// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"testing"

	"code.gitea.io/gitea/models/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectCycle(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// No cycle initially
	hasCycle, err := DetectCycle(ctx, 1)
	require.NoError(t, err)
	assert.False(t, hasCycle)

	// Create cycle: A -> B -> C -> A
	err = AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	require.NoError(t, err)
	err = AddDependency(ctx, 1, 2, 3, DepTypeBlocks)
	require.NoError(t, err)
	
	// This should fail due to cycle detection
	err = AddDependency(ctx, 1, 3, 1, DepTypeBlocks)
	require.Error(t, err)
	assert.True(t, IsErrCircularDependency(err))
}

func TestGetCyclePath(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// No cycle initially
	path, err := GetCyclePath(ctx, 1)
	require.NoError(t, err)
	assert.Nil(t, path)

	// Create a cycle manually (bypassing the check)
	// Note: In real usage, cycles are prevented by AddDependency
	// This test is for the detection function
	_, err = db.GetEngine(ctx).Insert(&IssueDependency{
		RepoID:    1,
		IssueID:   1,
		DependsOn: 2,
		DepType:   DepTypeBlocks,
	})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).Insert(&IssueDependency{
		RepoID:    1,
		IssueID:   2,
		DependsOn: 3,
		DepType:   DepTypeBlocks,
	})
	require.NoError(t, err)
	_, err = db.GetEngine(ctx).Insert(&IssueDependency{
		RepoID:    1,
		IssueID:   3,
		DependsOn: 1,
		DepType:   DepTypeBlocks,
	})
	require.NoError(t, err)

	path, err = GetCyclePath(ctx, 1)
	require.NoError(t, err)
	assert.NotNil(t, path)
	assert.Len(t, path, 3)
}