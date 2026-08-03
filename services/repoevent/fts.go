// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"context"
	"sync/atomic"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
)

// ftsIndex is one Postgres GIN index over a source's searchable text.
//
// Expr is built from TSVectorExpr rather than written out, so the index this package creates and
// the expression its queries are written with cannot be spelled differently - a one-character
// difference means the planner silently ignores the index. Migration 328 does spell the same four
// statements out as literals, because a migration may not call into code that is still moving;
// TestFTSIndexStatementsMatchMigration328 is what keeps that third copy honest.
type ftsIndex struct {
	Name  string
	Table string
	Expr  string
}

// CreateSQL renders the statement that builds this index. IF NOT EXISTS makes it idempotent, which
// is what lets it run on every startup rather than once.
func (i ftsIndex) CreateSQL() string {
	return "CREATE INDEX IF NOT EXISTS " + i.Name + " ON " + i.Table + " USING GIN (" + i.Expr + ")"
}

// ftsIndexes are the indexes that make ?q= an index lookup instead of a scan on Postgres.
var ftsIndexes = []ftsIndex{
	{"idx_action_content_fts", "action", TSVectorExpr("content")},
	{"idx_comment_content_fts", "comment", TSVectorExpr("content")},
	{"idx_review_content_fts", "review", TSVectorExpr("content")},
	{"idx_commit_status_fts", "commit_status", TSVectorExpr2("description", "context")},
}

// ftsIndexed records whether Init found all four indexes present and valid. It starts false, so a
// process that never ran Init - a CLI command, a unit test - searches through the LIKE path, which
// is correct on every dialect and needs no schema.
var ftsIndexed atomic.Bool

// Init makes ?q= usable on Postgres and decides which of the two search paths this process takes.
//
// It exists because migration 328 cannot serve a fresh install. Gitea skips every migration when
// the version record is absent (models/migrations/migrations.go: "it is a fresh installation, and
// we can skip all migrations") and creates the schema from the xorm structs instead, so raw DDL in
// a migration body only ever reaches installs that upgraded into it. Without this startup pass, an
// instance installed after this shipped would take the full-text branch against four indexes that
// were never created - strictly slower than the LIKE fallback it would otherwise have used, on
// every search request, forever, and silently: the rows come back correct.
//
// The migration is still the right place for the upgrade path. It runs once, inside a window the
// operator has already scheduled as downtime, where a multi-minute index build on a mature `action`
// table belongs. This pass then finds the indexes already there and costs one catalog lookup each.
//
// Failure is not fatal. An index that could not be created (no permission, a concurrent boot losing
// the race on the same name) leaves search on the LIKE path, which returns correct rows on any
// dialect; refusing to start the instance over a missing performance index would be the worse
// outcome. What must not happen is taking the full-text branch anyway, which is why the flag is set
// from what the catalog reports rather than from the dialect alone.
func Init(ctx context.Context) error {
	ftsIndexed.Store(ensureFTSIndexes(ctx))
	return nil
}

// ensureFTSIndexes creates the missing indexes and reports whether all four are usable afterwards.
func ensureFTSIndexes(ctx context.Context) bool {
	// GIN and to_tsvector are Postgres features; MySQL and SQLite answer ?q= through
	// db.BuildCaseInsensitiveLike and need no schema of their own, so there is nothing to do
	// and nothing to check.
	if !setting.Database.Type.IsPostgreSQL() {
		return false
	}
	for _, idx := range ftsIndexes {
		if _, err := db.GetEngine(ctx).Exec(idx.CreateSQL()); err != nil {
			log.Error("repoevent: cannot create %s: %v - repository event search falls back to LIKE", idx.Name, err)
			return false
		}
	}
	for _, idx := range ftsIndexes {
		// Present is not the same as usable: a build that failed part-way - the hazard
		// CREATE INDEX CONCURRENTLY carries, and the reason migration 328 does not use it -
		// leaves an INVALID index that the planner ignores and that IF NOT EXISTS then skips
		// forever. An operator repairs that with REINDEX INDEX <name>; until then, LIKE.
		valid, err := ftsIndexIsValid(ctx, idx.Name)
		if err != nil {
			log.Error("repoevent: cannot check %s: %v - repository event search falls back to LIKE", idx.Name, err)
			return false
		}
		if !valid {
			log.Warn("repoevent: %s is missing or invalid - repository event search falls back to LIKE; REINDEX INDEX %s repairs it", idx.Name, idx.Name)
			return false
		}
	}
	return true
}

// ftsIndexIsValid asks Postgres whether one index exists and finished building.
//
// to_regclass resolves the name through search_path, so an install using database.SCHEMA finds its
// own index rather than a same-named one elsewhere, and returns NULL - matching no row - when the
// index does not exist at all.
func ftsIndexIsValid(ctx context.Context, name string) (bool, error) {
	rows, err := db.GetEngine(ctx).Query("SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass(?)", name)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	// Drivers render a Postgres bool as "t" or as "true" depending on the wire format in use;
	// both mean the same thing and anything else means not valid.
	switch string(rows[0]["indisvalid"]) {
	case "t", "true":
		return true, nil
	default:
		return false, nil
	}
}

// UsesFullTextSearch reports whether ?q= runs as a Postgres full-text match rather than as a
// case-insensitive LIKE.
//
// Both conditions are load-bearing. The dialect decides whether the syntax exists at all; the flag
// decides whether taking that branch is an improvement. Without the second, an install whose
// indexes were never created - every fresh Postgres install before Init existed - would evaluate
// to_tsvector over every row of `action` and `comment` on every search, which is more expensive
// than the LIKE it replaced.
func UsesFullTextSearch() bool {
	return setting.Database.Type.IsPostgreSQL() && ftsIndexed.Load()
}
