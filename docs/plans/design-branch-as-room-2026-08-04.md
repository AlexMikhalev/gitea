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
3. Room-open is idempotent: look up the issue with the room's deterministic title (`Room: <branch>`) and the room
   marker for that branch — an indexed, `LIMIT`-ed query, never a walk over the repository's open issues — and
   create via the `repo.CreateIssue` path (api.go:1736-1737) only when the branch has never had a room. Marker =
   machine-readable JSON front-matter block at the top of the issue body, aligning with #39's convention. Title
   and marker together are the match predicate, and it lives in `modules/robotroom` so hook and CLI cannot drift:
   a room a human renamed is detached from the automation on *both* sides rather than one. Known limitation:
   search-then-create has no unique constraint, so two concurrent deliveries for the same new branch could create
   duplicate rooms; serialized webhook delivery keeps the window theoretical.
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
- **Two switches, both 404.** The route is gated on `[issue_graph] ENABLED` — the feature's master switch, which
  the three read-only robot routes already honour — before `ROOM_HOOK_SECRET`. The write path must not be the one
  route that keeps running after an operator has switched the feature off in an incident.
- **Webhook content type.** `json` is the documented choice; `form` works because the signature covers the JSON
  payload either way (deliver.go signs `t.PayloadContent`), and the payload is read from the parsed `payload`
  field — `sudo()` wraps the whole API router and its `ParseForm` has drained a urlencoded body before the handler
  runs. For a form delivery the 4 MiB cap therefore bounds the parsed field, not the read; `net/http`'s own 10 MB
  `ParseForm` limit is what bounded the request before that.
- The hook delivery carries no credentials; auth is the HMAC signature only. On instances with strict sign-in
  (`REQUIRE_SIGNIN_VIEW`, `Service.RequireSignInViewStrict`; checked in `verifyAuthWithOptions`,
  routers/api/v1/api.go:1043-1045) tokenless requests are rejected with 403 before the HMAC check runs — the room
  hook requires anonymous API access to be allowed.
- **`ROOM_HOOK_SECRET` is a master secret, not a webhook secret.** Each repository's webhook secret is
  `HMAC-SHA256(ROOM_HOOK_SECRET, "gitea-robot/room:v1:<owner>/<repo>")`, hex, owner and repo lower-cased. The
  signature is checked against the secret of the repository the payload *names*, so the repo admin who must be
  given a secret to configure the hook holds one that only works for their own repository; the master never
  leaves `app.ini`. Documented for operators in `docs/ROBOT_SECURITY.md`.
- **A signature authenticates the repository, never a user.** `sender` is an attribution preference: the resolved
  actor (sender → repo owner) must be an active individual account *with issue-write access to that repository*,
  or the delivery is refused with 403 before any write. There is deliberately no site-admin fallback for
  organization-owned repos — attributing automated content to the lowest-id site admin both misstates who acted
  and gives the hook far more reach than it needs.
- Every outcome is audited, failures included (`bad_signature`, `repo_not_found`, `actor_denied`, `doer_error`,
  `room_error`): the caller who is refused is more interesting to an operator than the one who succeeds.

## Acceptance criteria
- Push to `feat/foo` creates exactly one room issue; re-pushing never creates a duplicate (idempotent by marker).
- The "exactly one" holds for the life of the branch *name*: pushing a branch whose room was already closed
  (merged, deleted, or closed by hand) **reopens that room** rather than opening a second issue with the same
  title. The push response distinguishes the three outcomes (`created`, `reopened`, or neither).
- CI status change on the branch's head SHA posts one status comment on the room issue.
- Branch merge (pull_request closed+merged) or delete (delete event / zero-after push) closes the room issue;
  hook with bad/missing signature is rejected (401, or 403 under strict sign-in).
- `gitea-robot room open|status|close --owner X --repo Y --branch feat/foo` performs the same operations as the hook,
  including the reopen rule above: `room open` on a branch whose room is closed revives that issue, so the manual
  path cannot leave a repository with two rooms for one branch.

## Non-goals
No fork template backlink (YAGNI — only if the pilot proves value, per issue); no new tables or migrations; no
UI; no outbound webhooks/SSE; no Nostr relay publishing; no handling of non-`feat/*` branches; no PR rooms (F-scope).

## Test plan
- Unit (`routers/api/v1/robot/room_test.go`, beside robot_test.go): signature verify (good/bad/absent), event
  dispatch, feat/* ref filter, marker parse/idempotency, delete-vs-push handling.
- Unit (`cmd/gitea-robot/main_test.go` pattern): `room` subcommand arg validation and request shapes against an
  httptest fake server (wiremock-style; no live instance).
- Integration (`tests/integration/api_robot_room_test.go`, beside api_robot_test.go): full hook flow in-process —
  push event opens room, duplicate push is a no-op, status event comments, unsigned POST rejected, a delivery
  signed with another repository's derived secret rejected (401), a sender without issue-write access rejected
  (403), and a push after close reopening the same issue.

## Gates
`make fmt` · `make lint-go` · `make test-backend` · `make test-sqlite#TestAPIRobotRoom` · `make tidy` only if
`go.mod` changes. New `.go` files carry a 2026 copyright header; no trailing whitespace.
