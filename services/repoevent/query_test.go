// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"os"
	"strings"
	"testing"

	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/builder"
)

func sqlOf(t *testing.T, cond builder.Cond) (string, []any) {
	t.Helper()
	sql, args, err := builder.ToSQL(cond)
	require.NoError(t, err)
	return sql, args
}

func TestKeysetCondNoCursorIsUnbounded(t *testing.T) {
	sql, args := sqlOf(t, keysetCond(nil, KindAction, "created_unix", "id"))
	assert.Empty(t, strings.TrimSpace(sql))
	assert.Empty(t, args)
}

// The three branches are the whole reason a union paginator either works or repeats rows, so each
// is asserted against the ordering it comes from: created DESC, then kind ASC, then id DESC.
func TestKeysetCondPerKindRelationToCursor(t *testing.T) {
	cursor := &Cursor{CreatedUnix: timeutil.TimeStamp(500), Kind: KindComment, SourceID: 42}

	t.Run("kind sorting after the cursor still owes the cursor's own second", func(t *testing.T) {
		sql, args := sqlOf(t, keysetCond(cursor, KindReview, "created_unix", "id"))
		assert.Equal(t, "created_unix<=?", strings.ReplaceAll(sql, " ", ""))
		assert.Equal(t, []any{timeutil.TimeStamp(500)}, args)
	})

	t.Run("the cursor's own kind owes only the rows below it", func(t *testing.T) {
		sql, args := sqlOf(t, keysetCond(cursor, KindComment, "created_unix", "id"))
		assert.Contains(t, sql, "created_unix<?")
		assert.Contains(t, sql, "created_unix=?")
		assert.Contains(t, sql, "id<?")
		assert.Equal(t, []any{timeutil.TimeStamp(500), timeutil.TimeStamp(500), int64(42)}, args)
	})

	t.Run("kind sorting before the cursor is done with that second", func(t *testing.T) {
		sql, args := sqlOf(t, keysetCond(cursor, KindAction, "created_unix", "id"))
		assert.Equal(t, "created_unix<?", strings.ReplaceAll(sql, " ", ""))
		assert.Equal(t, []any{timeutil.TimeStamp(500)}, args)
	})
}

func ts(v int64) *timeutil.TimeStamp {
	stamp := timeutil.TimeStamp(v)
	return &stamp
}

func TestRangeCondIsInclusiveAndOptional(t *testing.T) {
	sql, _ := sqlOf(t, rangeCond(nil, nil, "created_unix"))
	assert.Empty(t, strings.TrimSpace(sql))

	sql, args := sqlOf(t, rangeCond(ts(10), nil, "created_unix"))
	assert.Equal(t, "created_unix>=?", strings.ReplaceAll(sql, " ", ""))
	assert.Equal(t, []any{timeutil.TimeStamp(10)}, args)

	sql, args = sqlOf(t, rangeCond(ts(10), ts(20), "created_unix"))
	assert.Contains(t, strings.ReplaceAll(sql, " ", ""), "created_unix>=?")
	assert.Contains(t, strings.ReplaceAll(sql, " ", ""), "created_unix<=?")
	assert.Equal(t, []any{timeutil.TimeStamp(10), timeutil.TimeStamp(20)}, args)
}

// The Unix epoch is a timestamp a caller can name, not a way of saying "no filter".
// `until=1970-01-01T00:00:00Z` asks for the empty stream; a zero-means-unset range would answer it
// with every event in the repository.
func TestRangeCondTreatsTheEpochAsAFilter(t *testing.T) {
	sql, args := sqlOf(t, rangeCond(nil, ts(0), "created_unix"))
	assert.Equal(t, "created_unix<=?", strings.ReplaceAll(sql, " ", ""))
	assert.Equal(t, []any{timeutil.TimeStamp(0)}, args)
}

func TestSearchCondEmptyQueryFiltersNothing(t *testing.T) {
	sql, _ := sqlOf(t, searchCond("   ", TSVectorExpr("content"), "content"))
	assert.Empty(t, strings.TrimSpace(sql))
}

func TestSearchCondFallsBackToCaseInsensitiveLike(t *testing.T) {
	defer withDatabaseType(t, "sqlite3")()
	require.False(t, UsesFullTextSearch())

	sql, args := sqlOf(t, searchCond("Hello", TSVectorExpr("content"), "content", "name"))
	assert.Contains(t, sql, "LOWER(content) LIKE ?")
	assert.Contains(t, sql, "LOWER(name) LIKE ?")
	assert.Equal(t, []any{"%hello%", "%hello%"}, args)
}

func TestSearchCondUsesFullTextOnPostgres(t *testing.T) {
	defer withDatabaseType(t, "postgres")()
	defer withFullTextIndexes(t, true)()
	require.True(t, UsesFullTextSearch())

	sql, args := sqlOf(t, searchCond("hello world", TSVectorExpr("content"), "content"))
	assert.Contains(t, sql, "to_tsvector('english'")
	// plainto_tsquery, not to_tsquery: to_tsquery parses its argument as an expression and
	// raises a syntax error on ordinary prose like this one.
	assert.Contains(t, sql, "plainto_tsquery('english', ?)")
	assert.Equal(t, []any{"hello world"}, args)
}

// A source with no index takes the LIKE path on every dialect. `q` is never dropped - the same
// rows come back, they just cost a scan.
func TestSearchCondWithoutIndexStillFiltersOnPostgres(t *testing.T) {
	defer withDatabaseType(t, "postgres")()
	defer withFullTextIndexes(t, true)()

	sql, args := sqlOf(t, searchCond("/api/v1/repos", "", "request_url"))
	assert.Contains(t, sql, "LOWER(request_url) LIKE ?")
	assert.Equal(t, []any{"%/api/v1/repos%"}, args)
}

// TestTSVectorExprMatchesMigration328 is what keeps the query expression and the index expression
// from drifting. Migration 328 spells the SQL out as a literal because a migration may not depend
// on code that is still moving; that duplication is only safe while something checks it, because a
// one-character difference means the index is silently not used.
//
// The columns here are the ones the adapters actually pass, table-qualified, because an assertion
// about TSVectorExpr("content") would check a string no call site produces. The migration cannot
// qualify - each index names one table - so the qualifier is stripped, and
// TestTSVectorExprOnlyVariesByColumn is what makes stripping it a sound step rather than a way of
// hiding a difference.
func TestTSVectorExprMatchesMigration328(t *testing.T) {
	source, err := os.ReadFile("../../models/migrations/v1_26/v328.go")
	require.NoError(t, err)
	migration := string(source)

	for _, expr := range []string{
		TSVectorExpr(unqualify("`action`.content")),
		TSVectorExpr(unqualify("`comment`.content")),
		TSVectorExpr(unqualify("`review`.content")),
		TSVectorExpr2(unqualify("`commit_status`.description"), unqualify("`commit_status`.context")),
	} {
		assert.Contains(t, migration, expr,
			"migration 328 does not index the expression services/repoevent queries with")
	}
}

// Qualifying a column must change nothing but the column, or the comparison above would be
// comparing an expression the adapters never build against an index they then cannot use.
func TestTSVectorExprOnlyVariesByColumn(t *testing.T) {
	assert.Equal(t, TSVectorExpr("content"),
		strings.ReplaceAll(TSVectorExpr("`action`.content"), "`action`.", ""))
	assert.Equal(t, TSVectorExpr2("description", "context"),
		strings.ReplaceAll(TSVectorExpr2("`commit_status`.description", "`commit_status`.context"), "`commit_status`.", ""))
}

// unqualify drops a `table`. prefix, so a test can compare an adapter's qualified expression with
// the single-table one the migration indexes.
func unqualify(col string) string {
	if i := strings.LastIndex(col, "."); i >= 0 {
		return col[i+1:]
	}
	return col
}

func withDatabaseType(t *testing.T, dbType string) func() {
	t.Helper()
	previous := setting.Database.Type
	setting.Database.Type = setting.DatabaseType(dbType)
	return func() { setting.Database.Type = previous }
}
