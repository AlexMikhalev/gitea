// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"context"
	"strings"
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

// CreateSQL renders the statement that builds this index. IF NOT EXISTS is what makes this
// statement and migration 328's copy of it harmless to each other, and covers the boot that loses
// a race for the same name against another one; it is not what decides whether to issue it at all -
// ensureFTSIndexes reads the catalog for that, because a skipped statement and a multi-hour
// blocking build are indistinguishable from here.
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

// ensureFTSIndexes reports whether all four indexes are present and usable, building the ones that
// are absent when the operator has left that enabled.
//
// The catalog is read before any DDL is issued, and that order is the point. CREATE INDEX IF NOT
// EXISTS is idempotent, but it is not cheap when the index really is missing: it is an ordinary
// blocking build, holding a SHARE lock on the table until it finishes, which is exactly what
// migration 328 argues at length belongs inside a window the operator scheduled. Issuing it
// unconditionally at every boot means any install whose indexes are gone for a reason other than
// "it is new" - an operator dropped them to reclaim disk, a restore that omitted them - pays that
// build at the next restart, with writes blocked for its duration and nothing in the log to say
// that a build rather than a catalog check was about to start. Checking first makes the ordinary
// case four catalog lookups, and makes the other case announce itself before it begins.
func ensureFTSIndexes(ctx context.Context) bool {
	// GIN and to_tsvector are Postgres features; MySQL and SQLite answer ?q= through the LIKE
	// path and need no schema of their own, so there is nothing to do and nothing to check.
	if !setting.Database.Type.IsPostgreSQL() {
		return false
	}

	status, err := ftsIndexStatuses(ctx)
	if err != nil {
		log.Error("repoevent: cannot read the full-text index catalog: %v - repository event search falls back to LIKE", err)
		return false
	}
	build, invalid := ftsIndexesToBuild(status)

	if len(invalid) > 0 {
		// Present is not the same as usable: a build that failed part-way - the hazard
		// CREATE INDEX CONCURRENTLY carries, and the reason migration 328 does not use it -
		// leaves an INVALID index that the planner ignores and that IF NOT EXISTS then skips
		// forever. Another CREATE would not repair it, so none is attempted.
		log.Warn("repoevent: %s exist but never finished building - repository event search falls back to LIKE; REINDEX INDEX <name> repairs them", ftsIndexNames(invalid))
		return false
	}
	if len(build) == 0 {
		return true
	}
	if !setting.Repository.EventStreamSearchIndexAutoBuild {
		log.Info("repoevent: %s are missing and [repository].EVENT_STREAM_SEARCH_INDEX_AUTO_BUILD is disabled - repository event search falls back to LIKE until they are created", ftsIndexNames(build))
		return false
	}

	// Said before the lock is taken rather than after it is released: an operator watching a
	// startup that has stopped moving needs this line to be already in the log.
	log.Warn("repoevent: %s are missing and will be built now. Each CREATE INDEX holds a SHARE lock on its table until it completes, which blocks writes to it - on a mature instance `action` and `comment` can take minutes to hours. To avoid this at startup, build them with CREATE INDEX CONCURRENTLY out of band and/or set [repository].EVENT_STREAM_SEARCH_INDEX_AUTO_BUILD = false.", ftsIndexNames(build))
	for _, idx := range build {
		if _, err := db.GetEngine(ctx).Exec(idx.CreateSQL()); err != nil {
			log.Error("repoevent: cannot create %s: %v - repository event search falls back to LIKE", idx.Name, err)
			return false
		}
	}

	// What was just built is read back rather than assumed, for the same reason the flag is not
	// set from the dialect alone: a statement that returned no error and an index the planner
	// will use are two different claims.
	if status, err = ftsIndexStatuses(ctx); err != nil {
		log.Error("repoevent: cannot read the full-text index catalog: %v - repository event search falls back to LIKE", err)
		return false
	}
	if remaining, stillInvalid := ftsIndexesToBuild(status); len(remaining) > 0 || len(stillInvalid) > 0 {
		log.Warn("repoevent: %s are still not usable after being built - repository event search falls back to LIKE", ftsIndexNames(append(remaining, stillInvalid...)))
		return false
	}
	log.Info("repoevent: built %s - repository event search uses full text", ftsIndexNames(build))
	return true
}

// ftsIndexesToBuild splits the four indexes by what the catalog said about them: the ones a CREATE
// would help, and the ones only a REINDEX can. An index nothing is known about is treated as
// missing, which is what the zero value of the map lookup gives.
func ftsIndexesToBuild(status map[string]ftsIndexStatus) (build, invalid []ftsIndex) {
	for _, idx := range ftsIndexes {
		switch status[idx.Name] {
		case ftsIndexValid:
		case ftsIndexInvalid:
			invalid = append(invalid, idx)
		default:
			build = append(build, idx)
		}
	}
	return build, invalid
}

// ftsIndexNames renders a set of indexes for a log line.
func ftsIndexNames(indexes []ftsIndex) string {
	names := make([]string, 0, len(indexes))
	for _, idx := range indexes {
		names = append(names, idx.Name)
	}
	return strings.Join(names, ", ")
}

// ftsIndexStatus is what the catalog says about one index.
//
// Missing and invalid are kept apart because the repair differs: a missing index is created, an
// invalid one has to be REINDEXed and another CREATE IF NOT EXISTS would silently do nothing.
type ftsIndexStatus int

const (
	ftsIndexMissing ftsIndexStatus = iota
	ftsIndexInvalid
	ftsIndexValid
)

// ftsIndexStatuses asks Postgres about all four indexes at once.
func ftsIndexStatuses(ctx context.Context) (map[string]ftsIndexStatus, error) {
	status := make(map[string]ftsIndexStatus, len(ftsIndexes))
	for _, idx := range ftsIndexes {
		one, err := ftsIndexStatusOf(ctx, idx.Name)
		if err != nil {
			return nil, err
		}
		status[idx.Name] = one
	}
	return status, nil
}

// ftsIndexStatusOf asks Postgres whether one index exists and finished building.
//
// to_regclass resolves the name through search_path, so an install using database.SCHEMA finds its
// own index rather than a same-named one elsewhere, and returns NULL - matching no row - when the
// index does not exist at all.
func ftsIndexStatusOf(ctx context.Context, name string) (ftsIndexStatus, error) {
	rows, err := db.GetEngine(ctx).Query("SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass(?)", name)
	if err != nil {
		return ftsIndexMissing, err
	}
	if len(rows) == 0 {
		return ftsIndexMissing, nil
	}
	// Drivers render a Postgres bool as "t" or as "true" depending on the wire format in use;
	// both mean the same thing and anything else means the index is there but not usable.
	switch string(rows[0]["indisvalid"]) {
	case "t", "true":
		return ftsIndexValid, nil
	default:
		return ftsIndexInvalid, nil
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
