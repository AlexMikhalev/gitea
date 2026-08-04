# Design Gate — Branch-as-room automation (issue #56, epic #53, phase F3)

## Problem
Work on a `feat/*` branch has no single place where agents and humans see its live state: the branch, its CI
status, and its discussion are scattered across commits, commit statuses, and ad-hoc issues. F3 adds **branch-as-room**:
one idempotent "room" issue per `feat/*` branch, opened on first push, updated with CI status comments, closed when
the branch merges or is deleted. Automation-side only (gitea-robot + one inbound hook route); no fork-core schema change.

## Decision — exact touchpoints
1. `routers/api/v1/robot/room.go` (new) — server-side room logic + `POST /api/v1/robot/room/hook`, registered in the
   robot group in `routers/api/v1/api.go` (:2063-2076, beside `triage`/`ready`/`graph`). Receives Gitea webhook
   deliveries signed with `X-Gitea-Signature` (raw hex HMAC-SHA256) or `X-Hub-Signature-256` (`sha256=`-prefixed
   hex; services/webhook/deliver.go:136-143); secret from app.ini, unconfigured secret = route disabled (404),
   never an unsigned accept.
2. Hook dispatch on `X-Gitea-Event`: `push` (modules/webhook/type.go:14 `HookEventPush`) → parse `PushPayload`,
   for each `refs/heads/feat/*` ref call room-open (zero `after` SHA = branch deleted → room-close); `delete`
   (`HookEventDelete`) → room-close for the deleted feat/* branch; `pull_request` (`HookEventPullRequest`) →
   closed+merged PR from a feat/* branch → room-close; `status` (type.go:34 `HookEventStatus`) → parse
   `CommitStatusPayload` (modules/structs/hook.go:568), resolve SHA → room of the feat/* branch whose head is the
   SHA (marker-head match as git-free fallback), append status comment. All other events: 202 + ignore.
3. Room-open is idempotent: search open issues (`GET`-equivalent of `repo.SearchIssues`, api.go:1732) for a body
   containing the room marker for that branch; create via the `repo.CreateIssue` path (api.go:1736-1737) only when
   absent. Marker = machine-readable JSON front-matter block at the top of the issue body, aligning with #39's
   convention. Known limitation: search-then-create has no unique constraint, so two concurrent deliveries for the
   same new branch could create duplicate rooms; serialized webhook delivery keeps the window theoretical.
4. `cmd/gitea-robot/main.go` — `room` subcommand (`open|status|close` actions) in the `switch command` dispatch
   (:167-181) + `printUsage()` (:185), reusing `apiGet`/`apiPostSafe` (:349, :1002) and `setRequestAuth` (:50) so
   NIP-98 agent identity works unchanged; plus a `room` MCP tool beside `handleTriageTool`/`handleAddDepTool` (:737, :904).
   CLI is the manual/ops path for the same endpoints the hook drives automatically.

## Ground truth — verified answers (recorded after implementation)
- **#39's front-matter convention:** no shipped #39 artifact exists in this repo to confirm against (the only
  front-matter code is YAML metadata for markdown rendering, modules/markup/markdown/meta.go). The fenced
  ```json block `{"type":"gitea-robot/room","version":1,"branch":...,"head":...}` is the room convention going
  forward; the parser is deliberately tolerant of human edits around the block.
- **`CommitStatusPayload` field shape (hook.go:569):** carries `sha`/`state`/`context`/`description`/`target_url`
  and **no branch ref**. Branch is recovered from the commit graph: the feat/* branch whose head is the SHA
  (containment is deliberately *not* enough — a status for an older commit would otherwise fan out to every room
  whose branch contains it); fallback is the head recorded in the room marker, which works when the git repo is
  unavailable to the hook process. The marker head is refreshed on every push of the branch, so the fallback
  tracks the branch's current tip rather than only the creation-time head.
- **Webhook secret delivery format (deliver.go:136-143):** raw hex HMAC-SHA256 in `X-Gitea-Signature`;
  `sha256=`-prefixed hex in `X-Hub-Signature-256`. Both are accepted; the empty signature never is.
- **Branch deletion:** `push` payloads carry **no `deleted` flag** (hook.go:203); deletion surfaces either as a
  push with an all-zero `after` SHA or as the dedicated `delete` event. Both paths close the room.
- **Merge without delete:** a feat/* branch merged via PR but not deleted fires `pull_request` closed with
  `merged: true` and the head branch in `pull_request.head.ref` — the hook closes the room on this event.

## Deployment constraints
- The hook delivery carries no credentials; auth is the HMAC signature only. On instances with strict sign-in
  (`REQUIRE_SIGNIN_VIEW`, `Service.RequireSignInViewStrict`; checked in `verifyAuthWithOptions`,
  routers/api/v1/api.go:1043-1045) tokenless requests are rejected with 403 before the HMAC check runs — the room
  hook requires anonymous API access to be allowed.
- Room mutations are attributed to the webhook sender; if the sender is not a local user, the repo owner acts,
  and for organization-owned repos (an org cannot author issues) a site admin acts instead.

## Acceptance criteria
- Push to `feat/foo` creates exactly one room issue; re-pushing never creates a duplicate (idempotent by marker).
- CI status change on the branch's head SHA posts one status comment on the room issue.
- Branch merge (pull_request closed+merged) or delete (delete event / zero-after push) closes the room issue;
  hook with bad/missing signature is rejected (401, or 403 under strict sign-in).
- `gitea-robot room open|status|close --owner X --repo Y --branch feat/foo` performs the same operations as the hook.

## Non-goals
No fork template backlink (YAGNI — only if the pilot proves value, per issue); no new tables or migrations; no
UI; no outbound webhooks/SSE; no Nostr relay publishing; no handling of non-`feat/*` branches; no PR rooms (F-scope).

## Test plan
- Unit (`routers/api/v1/robot/room_test.go`, beside robot_test.go): signature verify (good/bad/absent), event
  dispatch, feat/* ref filter, marker parse/idempotency, delete-vs-push handling.
- Unit (`cmd/gitea-robot/main_test.go` pattern): `room` subcommand arg validation and request shapes against an
  httptest fake server (wiremock-style; no live instance).
- Integration (`tests/integration/api_robot_room_test.go`, beside api_robot_test.go): full hook flow in-process —
  push event opens room, duplicate push is a no-op, status event comments, unsigned POST rejected.

## Gates
`make fmt` · `make lint-go` · `make test-backend` · `make test-sqlite#TestAPIRobotRoom` · `make tidy` only if
`go.mod` changes. New `.go` files carry a 2026 copyright header; no trailing whitespace.
