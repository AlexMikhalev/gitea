// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"strings"
	"testing"

	"code.gitea.io/gitea/models/migrations/base"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
)

func Test_AddAgentIdentity(t *testing.T) {
	// The *pre-migration* schema, deliberately: a user table with no is_agent column, and none of
	// the three agent tables. Syncing the post-migration shape here instead would make every
	// assertion below pass with AddAgentIdentity replaced by `return nil`, which is the one thing
	// a migration test must not do.
	type User struct {
		ID   int64 `xorm:"pk autoincr"`
		Name string
	}

	x, deferrable := base.PrepareTestEnv(t, 0, new(User))
	defer deferrable()

	// Establish that the starting point really is the pre-migration schema, so what follows is
	// observing the migration rather than the fixture.
	hasIsAgent, err := columnExists(x, "user", "is_agent")
	require.NoError(t, err)
	require.False(t, hasIsAgent, "the test env must start before the migration, not after it")
	for _, table := range []string{"agent_key", "agent_audit_event", "agent_used_event"} {
		exists, err := x.IsTableExist(table)
		require.NoError(t, err)
		require.False(t, exists, "table %s must not exist before the migration runs", table)
	}

	// A row that predates the migration, so the added column can be observed on existing data
	// rather than only on the schema.
	_, err = x.Insert(&User{Name: "someone"})
	require.NoError(t, err)

	require.NoError(t, AddAgentIdentity(x))

	// The column the migration exists for. Without it services/auth cannot tell an agent from any
	// other user and every signed request is refused - and no other assertion here would notice.
	hasIsAgent, err = columnExists(x, "user", "is_agent")
	require.NoError(t, err)
	assert.True(t, hasIsAgent, "user.is_agent was not added")

	for _, table := range []string{"agent_key", "agent_audit_event", "agent_used_event"} {
		exists, err := x.IsTableExist(table)
		require.NoError(t, err)
		assert.True(t, exists, "table %s should exist", table)
	}

	// The pre-existing row has to come out of the migration with a usable value rather than
	// NULL: is_agent is declared NOT NULL DEFAULT false and is read on every auth path. Anything
	// but false here would enrol every account on the instance as an agent.
	type UserAfter struct {
		ID      int64 `xorm:"pk autoincr"`
		Name    string
		IsAgent bool
	}
	migrated := new(UserAfter)
	has, err := x.Table("user").Where("name = ?", "someone").Get(migrated)
	require.NoError(t, err)
	require.True(t, has)
	assert.False(t, migrated.IsAgent, "a pre-existing user must not come out of the migration as an agent")

	// Re-running a migration has to be a no-op: Gitea replays them on every start-up path that
	// finds an older schema version, and an operator who restores a backup replays them again.
	require.NoError(t, AddAgentIdentity(x))

	hasIsAgent, err = columnExists(x, "user", "is_agent")
	require.NoError(t, err)
	assert.True(t, hasIsAgent, "re-running the migration must not drop the column")

	// The replay guard is only a guard if the database refuses a second row for one event id.
	// Nothing is synced first, so the unique index can only have come from the migration.
	_, err = x.Insert(&AgentUsedEvent{EventID: "a1", ExpiresUnix: 1})
	require.NoError(t, err)
	_, err = x.Insert(&AgentUsedEvent{EventID: "a1", ExpiresUnix: 2})
	require.Error(t, err, "agent_used_event.event_id must be unique")

	// The audit row has to carry the signed event, not just a summary of it: without sig and the
	// other serialization inputs, event_id is an identifier no reader can check the row against.
	// Asserted on the live schema for the same reason as is_agent above.
	for _, column := range []string{"sig", "nonce", "event_tags", "event_content", "event_kind", "event_created_unix"} {
		exists, err := columnExists(x, "agent_audit_event", column)
		require.NoError(t, err)
		assert.True(t, exists, "agent_audit_event.%s was not added", column)
	}

	count, err := x.Count(new(AgentKey))
	require.NoError(t, err)
	require.EqualValues(t, 0, count)
}

// columnExists reads the column back off the live schema. Asking the database rather than xorm's
// model metadata is the point: it is the only way to tell an added column from a struct field
// that merely claims one, and it works the same on sqlite, MySQL and PostgreSQL.
func columnExists(x *xorm.Engine, tableName, columnName string) (bool, error) {
	tables, err := x.DBMetas()
	if err != nil {
		return false, err
	}
	for _, table := range tables {
		if !strings.EqualFold(table.Name, tableName) {
			continue
		}
		for _, col := range table.ColumnsSeq() {
			if strings.EqualFold(col, columnName) {
				return true, nil
			}
		}
	}
	return false, nil
}
