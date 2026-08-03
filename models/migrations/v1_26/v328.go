// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"code.gitea.io/gitea/modules/setting"

	"xorm.io/xorm"
)

// repoEventFTSIndexes are the GIN indexes that make ?q= on the unified repository event stream an
// index lookup instead of a scan.
//
// The expressions are written out here rather than built by services/repoevent, which builds the
// identical strings for its queries. A migration has to keep meaning on every future install what
// it meant the day it was written, so it may not call into code that is still moving; the price is
// this duplication, and services/repoevent's TestTSVectorExprMatchesMigration328 is what stops the
// two copies from drifting. A query whose expression differs from the index's by one character
// simply does not use the index - silently, and only in production.
//
// The left(..., 100000) is not cosmetic either. to_tsvector raises an error on strings over 1 MB,
// and comment.content is LONGTEXT: an unbounded expression index would turn "someone posted a very
// long comment" into "the INSERT fails", which is a much worse outcome than "the tail of a very
// long comment is not searchable".
var repoEventFTSIndexes = []string{
	"CREATE INDEX IF NOT EXISTS idx_action_content_fts ON action USING GIN (to_tsvector('english', left(coalesce(content, ''), 100000)))",
	"CREATE INDEX IF NOT EXISTS idx_comment_content_fts ON comment USING GIN (to_tsvector('english', left(coalesce(content, ''), 100000)))",
	"CREATE INDEX IF NOT EXISTS idx_review_content_fts ON review USING GIN (to_tsvector('english', left(coalesce(content, ''), 100000)))",
	"CREATE INDEX IF NOT EXISTS idx_commit_status_fts ON commit_status USING GIN (to_tsvector('english', left(coalesce(description, '') || ' ' || coalesce(context, ''), 100000)))",
}

// AddRepoEventFullTextIndexes creates the Postgres full-text indexes the unified repository event
// stream searches through, on installs that upgrade into this version.
//
// It is half of the story and cannot be the whole of it: Gitea skips every migration when the
// version record is absent, because a fresh database gets its schema from the xorm structs
// instead (migrations.go: "it is a fresh installation, and we can skip all migrations"). Raw DDL
// in a migration body therefore never runs on a new install. services/repoevent.Init issues the
// same four statements at startup for that reason, and IF NOT EXISTS is what makes the two
// harmless to each other. This one still exists because an upgrade is where a multi-minute index
// build on a mature `action` table belongs - inside a window the operator scheduled - rather than
// at the first boot after it.
//
// It is a no-op on every other dialect, and deliberately so: GIN and to_tsvector are Postgres
// features, and MySQL and SQLite have no equivalent that is worth a schema change here. Those
// installs answer ?q= through db.BuildCaseInsensitiveLike instead. What must not happen is a
// migration that fails an upgrade on the two dialects that cannot run it, so the dialect test
// comes before any SQL is issued rather than being left to an error handler afterwards.
//
// Re-running is safe: IF NOT EXISTS makes each statement idempotent, which matters because a
// migration that half-applied and was retried must converge rather than fail on the first index it
// already created.
//
// Cost, stated rather than discovered in production: each CREATE INDEX takes a SHARE lock on its
// table for the whole build, which blocks writes to it. `action` and `comment` are the two largest
// tables on a mature instance, so on an install with tens of millions of rows the four builds
// together can run for minutes to hours, and the instance is down for writes for that whole time.
//
// CONCURRENTLY would avoid the lock, and could be used here - the runner hands each migration the
// *xorm.Engine and wraps nothing in a transaction (migrations.go: `func (m *migration) Migrate` is
// a call to m.migrate and nothing else), and these statements go out through x.Exec, so the one
// thing CONCURRENTLY cannot tolerate is absent. It is still not used, for a different reason: a
// concurrent build that fails leaves the index behind marked INVALID, the planner ignores an
// INVALID index, and IF NOT EXISTS then skips it on every subsequent run - so the failure mode is
// an index that exists, is never used and is never rebuilt, which is exactly the silent
// degradation this migration exists to prevent. A blocking build either succeeds or leaves
// nothing. That trade is worth it for a step that runs once, at a startup the operator has already
// scheduled as downtime.
//
// An operator who cannot afford the pause can create these four indexes CONCURRENTLY by hand
// before upgrading; IF NOT EXISTS then makes this migration a no-op. If one of those builds fails,
// REINDEX INDEX <name> is the repair - and until it is run, services/repoevent notices the INVALID
// index at startup and keeps searching through LIKE rather than through a plan that cannot use it.
func AddRepoEventFullTextIndexes(x *xorm.Engine) error {
	if !setting.Database.Type.IsPostgreSQL() {
		return nil
	}
	for _, stmt := range repoEventFTSIndexes {
		if _, err := x.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
