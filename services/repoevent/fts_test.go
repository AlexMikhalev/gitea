// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFTSIndexStatementsMatchMigration328 pins the whole CREATE INDEX statement, not just the
// expression inside it.
//
// The index this package creates at startup and the one migration 328 creates on upgrade have to
// be the same index, or an upgraded install and a fresh one would answer ?q= through different
// plans - and a name that differs by a character means the startup pass builds a second index next
// to the migration's rather than finding it already there.
func TestFTSIndexStatementsMatchMigration328(t *testing.T) {
	source, err := os.ReadFile("../../models/migrations/v1_26/v328.go")
	require.NoError(t, err)
	migration := string(source)

	require.Len(t, ftsIndexes, 4, "migration 328 creates four indexes")
	for _, idx := range ftsIndexes {
		assert.Contains(t, migration, idx.CreateSQL(),
			"migration 328 does not create the index services/repoevent creates at startup")
	}
}

// A fresh Postgres install has no migration history, so migration 328 never runs on it and the four
// indexes do not exist. Deciding the search path from the dialect alone would send exactly those
// installs down the full-text branch with nothing to look the query up in - to_tsvector evaluated
// over every row of `action` and `comment`, on every request, which is worse than the LIKE it
// replaced. The flag is what stops that, so it is asserted here rather than left implied.
func TestUsesFullTextSearchNeedsTheIndexesToExist(t *testing.T) {
	for _, tc := range []struct {
		dbType  string
		indexed bool
		want    bool
	}{
		{"postgres", true, true},
		{"postgres", false, false},
		{"sqlite3", false, false},
		// A dialect that has no GIN cannot use one however the flag was left.
		{"sqlite3", true, false},
		{"mysql", true, false},
	} {
		t.Run(tc.dbType+"/indexed="+map[bool]string{true: "yes", false: "no"}[tc.indexed], func(t *testing.T) {
			defer withDatabaseType(t, tc.dbType)()
			defer withFullTextIndexes(t, tc.indexed)()

			assert.Equal(t, tc.want, UsesFullTextSearch())
		})
	}
}

// Init runs on every startup, including on MySQL and SQLite where there is no index to create. It
// must issue no SQL there at all - this test has no database engine set up, so a statement would
// panic rather than merely be wrong - and must leave the flag down so search takes the LIKE path.
func TestInitIssuesNothingOnOtherDialects(t *testing.T) {
	defer withFullTextIndexes(t, true)()
	for _, dbType := range []string{"sqlite3", "mysql", "mssql"} {
		t.Run(dbType, func(t *testing.T) {
			defer withDatabaseType(t, dbType)()

			require.NoError(t, Init(t.Context()))
			assert.False(t, ftsIndexed.Load())
			assert.False(t, UsesFullTextSearch())
		})
	}
}

// withFullTextIndexes sets the "the GIN indexes are there" flag for one test, standing in for the
// startup pass that would have set it against a real Postgres.
func withFullTextIndexes(t *testing.T, indexed bool) func() {
	t.Helper()
	previous := ftsIndexed.Load()
	ftsIndexed.Store(indexed)
	return func() { ftsIndexed.Store(previous) }
}
