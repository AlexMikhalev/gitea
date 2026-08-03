# Design Gate — Unified repo event stream + FTS (issue #55, epic #53, phase F2)

## Problem
A repo's activity lives in five tables with five shapes: `action` (models/activities/action.go:135), issue comments,
reviews, commit statuses, and F1's `agent_audit_event` (models/agent/audit.go:46). "What happened here since X" means
polling five paginated endpoints and merging client-side, with no text search over any of it. F2 is **read-side
only**: one cursor-paginated union endpoint plus optional FTS — no new storage, no write path, no schema change.

## Decision — exact touchpoints
1. `services/repoevent/` (new pkg) — the union. Per-source adapter `fetch(ctx, repoID, since, until, limit)` →
   `[]Event{Kind, SourceID, ActorID, CreatedUnix, RepoID, Title, Payload}`, merge-sorted on `(created_unix DESC, kind,
   source_id)`. Each adapter takes `LIMIT n`, the merge keeps the global top-n: no source is read in full, no noisy
   source starves the others.
2. Cursor = opaque base64 of `{created_unix, kind, source_id}` — the sort key, not an offset. Keyset, because OFFSET
   over a five-way union re-reads every skipped row and drifts when a source inserts mid-scan.
3. `routers/api/v1/repo/event.go` (new) — `GET /repos/{o}/{r}/events?kinds=&since=&until=&actor=&q=&cursor=&limit=`,
   registered in `routers/api/v1/api.go` beside the audit route (:1381), guarded `reqToken()` + `reqRepoReader(...)`;
   response `{data, next_cursor}`; swagger `modules/structs/repo_event.go` (new) + `routers/api/v1/swagger/repo.go`.
4. **Visibility** — `action.IsPrivate` (:148) and comment/review visibility are *not* implied by repo read access.
   Every adapter filters to what the requesting doer may see; private rows must not leak through the union.
5. `models/migrations/v1_26/v328.go` + `migrations.go:405` — `newMigration(328, ...)`; 327 (`AddAgentIdentity`) is the
   current max. Guarded: create the GIN index only when `setting.Database.Type.IsPostgreSQL()`
   (modules/setting/database.go:223), else no-op — it must not fail an install on MySQL/SQLite. A migration only
   reaches installs that *upgrade*: `migrations.go` skips every one of them on a fresh database, so
   `services/repoevent.Init` (called from `routers/init.go`) issues the same `CREATE INDEX IF NOT EXISTS`
   statements at startup, and decides the search path from whether the indexes are actually there rather than
   from the dialect alone. `?q=` uses `plainto_tsquery` when they exist and `db.BuildCaseInsensitiveLike`
   (models/db/common.go:19) when they do not. Never silently drop `q` — but the two paths do not match the same
   rows (stemmed lexemes vs raw substrings), which is a documented, deployment-dependent behaviour of the `q`
   parameter rather than an equivalence.
6. `cmd/gitea-robot/main.go` — `events` subcommand in the `switch command` dispatch (:167-181) and `printUsage()`,
   plus an `events` MCP tool beside `triage`/`ready`/`graph`/`add_dep` (:603-662).

## Ground truth to verify before writing adapters (do not assume)
- `ActionType` is `iota + 1`, 27 values, `ActionCreateRepo=1` … `ActionAutoMergePullRequest=27` (action.go:38-64).
- `CommentType` is `iota` from 0 (models/issues/comment.go:58-64): `CommentTypeComment=0`, `CommentTypeReview=22`.
  Only content-bearing types surface as events; system comments do not.
- `ReviewType` is `iota` from 0, `ReviewTypePending=0` (models/issues/review.go:94-102) — **pending reviews are
  unpublished and must be excluded.** `CommitStatusState` is a *string* enum, not an int:
  pending/success/error/failure/warning/skipped (modules/commitstatus/commit_status.go:12-22).
- Reuse `FindAuditEvents` (models/agent/audit.go:255); confirm the real index on `action(repo_id, created_unix)`
  before relying on the keyset scan.

## Acceptance criteria
- Endpoint merge-sorts all five kinds; `kinds`/`since`/`until`/`actor` filter as documented.
- Paging by `next_cursor` yields every event exactly once — no duplicates, no skips — including when rows are
  inserted between pages.
- `?q=` returns matches on Postgres (index path) and on SQLite/MySQL (ILIKE path).
- Rows a doer cannot see are absent; an unreadable repo returns 404. Asserted against a database, per gate, not
  as rendered SQL: `services/repoevent/list_db_test.go` and `tests/integration/api_repo_event_test.go`.
- Migration 328 applies cleanly on all three dialects and is idempotent on re-run; a *fresh* Postgres install,
  which never runs it, still ends up with the four indexes through `repoevent.Init`.
- `gitea-robot events` and the `events` MCP tool return the same data as the endpoint.

## Non-goals
No new event table or write path; no webhook/SSE/WebSocket push; no cross-repo or instance-wide feed; no Nostr relay
publishing (F3); no UI; no backfill; no change to existing activity/comment/review endpoints; no external search
engine — Postgres FTS with ILIKE fallback only.

## Test plan
- Unit (`services/repoevent`): merge ordering and stability, cursor round-trip, per-source `limit` fairness, each
  filter, empty and single-source streams. Adapter tests assert the enum mappings above, so an upstream enum change
  breaks a test.
- Integration (`tests/integration/api_repo_events_test.go`, beside `api_agent_auth_test.go`): full pagination sweep
  asserting exactly-once; permission matrix (anon / non-member / member / admin) over public and private repos; `?q=`
  on the fallback path. No wiremock — in-process HTTP against fixtures.
- The Postgres FTS path is **not** covered by the sqlite run; it needs `make test-pgsql`. Say so in the PR.

## Gates
`make fmt` · `make lint-go` · `make test-backend` · `make test-sqlite#TestAPIRepoEvents` · `make test-pgsql` for FTS ·
`make tidy` only if `go.mod` changes. New `.go` files carry a 2026 copyright header; no trailing whitespace.
