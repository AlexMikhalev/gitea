// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"testing"

	"code.gitea.io/gitea/models/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddDependency(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// Test adding a dependency
	err := AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	require.NoError(t, err)

	// Verify it was added
	deps, err := GetDependencies(ctx, 1, 1)
	require.NoError(t, err)
	assert.Len(t, deps, 1)
	assert.Equal(t, int64(2), deps[0].DependsOn)
	assert.Equal(t, DepTypeBlocks, deps[0].DepType)
}

func TestAddDependencyDuplicate(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// Add first dependency
	err := AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	require.NoError(t, err)

	// Try to add duplicate
	err = AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	assert.True(t, IsErrDependencyAlreadyExists(err))
}

func TestAddDependencyCircular(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// Add A blocks B
	err := AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	require.NoError(t, err)

	// Add B blocks C
	err = AddDependency(ctx, 1, 2, 3, DepTypeBlocks)
	require.NoError(t, err)

	// Try to add C blocks A (circular)
	err = AddDependency(ctx, 1, 3, 1, DepTypeBlocks)
	assert.True(t, IsErrCircularDependency(err))
}

func TestRemoveDependency(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// Add dependency
	err := AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	require.NoError(t, err)

	// Remove it
	err = RemoveDependency(ctx, 1, 1, 2)
	require.NoError(t, err)

	// Verify it was removed
	deps, err := GetDependencies(ctx, 1, 1)
	require.NoError(t, err)
	assert.Len(t, deps, 0)
}

func TestGetDependents(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// Add A blocks B
	err := AddDependency(ctx, 1, 2, 1, DepTypeBlocks)
	require.NoError(t, err)

	// Get dependents of A
	dependents, err := GetDependents(ctx, 1, 1)
	require.NoError(t, err)
	assert.Len(t, dependents, 1)
	assert.Equal(t, int64(2), dependents[0].IssueID)
}

func TestDeleteAllDependenciesForIssue(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	ctx := db.DefaultContext

	// Add multiple dependencies
	err := AddDependency(ctx, 1, 1, 2, DepTypeBlocks)
	require.NoError(t, err)
	err = AddDependency(ctx, 1, 3, 1, DepTypeBlocks)
	require.NoError(t, err)

	// Delete all for issue 1
	err = DeleteAllDependenciesForIssue(ctx, 1, 1)
	require.NoError(t, err)

	// Verify all were removed
	deps, err := GetDependencies(ctx, 1, 1)
	require.NoError(t, err)
	assert.Len(t, deps, 0)

	dependents, err := GetDependents(ctx, 1, 1)
	require.NoError(t, err)
	assert.Len(t, dependents, 0)
}