# Design Gate — NIP-98 agent identity + signed audit trail (issue #54, epic #53)

## Problem
Agent mutations (gitea-robot, Buzz) use a bearer PAT (`Authorization: token …`, cmd/gitea-robot/main.go:185,211,846):
replayable, bound to no method/URL/body, audited only as its owner. F1 adds an additive path — the agent signs each
request with a human-owned Nostr key (NIP-98 kind 27235, NIP-OA ownership chain), and each authenticated request
appends to an audit table.

## Decision — exact touchpoints
1. `models/user/user.go` — `IsAgent bool \`xorm:"NOT NULL DEFAULT false"\`` beside `ProhibitLogin` (:131). **Not** a new
   `UserType`: `UserTypeBot=4` already exists (:63) and the type is a persisted enum ordinal — widening it churns every
   `switch u.Type`. A flag is additive and composes with Bot.
2. `models/migrations/v1_26/v327.go` + `migrations.go:406` — `newMigration(327, "Add is_agent and agent identity
   tables", v1_26.AddAgentIdentity)`. Max today is 326 (`AddGraphCache`); one migration adds the column and all three
   tables.
3. `models/agent/` (new pkg) — `agent_key{ID, OwnerUserID, AgentUserID, PubKey(32-byte hex, unique), Npub, Scope,
   CreatedUnix, RevokedUnix}`; hex is the lookup key (NIP-98 carries hex), npub is display-only.
   `agent_audit_event` is insert + one outcome update + select. `agent_used_event` is the replay guard (see below).
4. `modules/nostr/` (new pkg) — the NIP-01 event envelope, its canonical serialization and id, BIP-340 sign/verify,
   and NIP-19 bech32 npub/nsec. See "Dependency seam" for why this is in-tree rather than `github.com/nbd-wtf/go-nostr`.
5. `services/agentauth/nip98.go` (new) — parse `Authorization: Nostr <base64 kind-27235 event>`; assert `kind==27235`,
   `created_at` within ±60s, `u` tag == absolute request URL, `method` tag == request method, signature; then, as a
   **separate second call**, `payload` tag == hex SHA-256 of the body. See "Verification is two-phase".
6. `services/auth/nostr.go` (new) — implements `auth.Method` verbatim per services/auth/interface.go:20-31; returns
   `(nil, nil)` when the header is missing or not `Nostr `-prefixed (never an error, else it shadows OAuth2/Basic).
   Registered in `buildAuthGroup()` at routers/api/v1/api.go.
7. `routers/api/v1/agent/key.go` (new); `POST|GET /agent/keys`, `DELETE /agent/keys/{id}` and
   `GET /repos/{owner}/{repo}/agent-audit` registered in `routers/api/v1/api.go`, plus the `recordAgentAudit`
   middleware.
8. `cmd/gitea-robot/main.go` — `GITEA_NOSTR_KEY` (nsec or hex) set ⇒ NIP-98 header on **every** request; unset ⇒
   today's PAT path byte-for-byte unchanged.
9. `services/cron/tasks_basic.go` — hourly `cleanup_agent_used_events`, so the replay guard's table is bounded.

## The three properties this rests on

### Scope — a signed request must never out-rank a PAT
`routers/api/v1/api.go`'s `tokenRequiresScopes` (:304-315) early-returns unless `ctx.Data["IsApiToken"] == true` **and**
`ApiTokenScope` is set. An auth method that omits those two therefore makes every scope guard in the API a no-op for
its requests — i.e. its credential would be strictly more powerful than any personal access token of the same user.
`agent_key.Scope` is mandatory (registration refuses an empty or unparsable scope, exactly as `POST /users/{u}/tokens`
does), and `services/auth/nostr.go` publishes it under those two keys. A key that somehow has no scope is refused at
authentication time as well. Pinned by `TestAPIAgentKeyScopeIsEnforced` and by the unit test asserting the two store
keys and the narrowness of the scope.

### Replay — binding stops repurposing, not repetition
The `u`/`method`/`payload` tags stop a captured header being aimed at a *different* request; on their own they do
nothing about the *same* request being sent twice inside the ±60s window, which duplicates whatever mutation it
carried. NIP-98 expects the server to retain event ids, so `agent_used_event` does: `event_id` carries a UNIQUE index
and `ConsumeEvent` spends it once, the database picking the winner between concurrent replays (a check-then-insert
could not). `agent_audit_event.event_id` is UNIQUE too, so two rows for one event would mean the guard had been
bypassed. Rows expire at `created_at + clock skew` and the hourly cron deletes them.

### Enrolment is a human act
`reqToken()` only asks whether *someone* is signed in, and a NIP-98 request is signed in — so it does not keep an agent
out of the key endpoints. `reqHumanAuth()` does, by refusing any request whose `AuthedMethod` is `nostr`. Without it a
single leaked key mints unlimited siblings and revoking the leaked one accomplishes nothing.

The same argument covers every *other* account-level credential, so `reqHumanAuth()` guards those too: `/user/keys`
(SSH), `/user/applications/oauth2`, and `/user/gpg_keys` + `/user/gpg_key_verify`. Each of those outlives the signing
key that created it, so leaving them open would have made revocation a half-undo by another route — a leaked key
carrying `write:user` could plant an SSH key or mint an OAuth2 application and keep its access after the Nostr key was
withdrawn. `POST /users/{username}/tokens` needs no guard: it sits behind `reqBasicOrRevProxyAuth()`, which a signature
cannot satisfy. Containment stops there on purpose — it is about credentials, not about narrowing what a
correctly-scoped agent may do inside a repository or organization.

## Verification is two-phase
`agentauth.VerifyCredential` decides everything the `Authorization` header alone can and **does not read the body**;
`(*SignedRequest).VerifyPayload` reads it. `services/auth` runs the key lookup between the two. This ordering is the
whole point: every header-only check costs a fixed few hundred bytes, whereas buffering the body costs up to
`MaxBodySize` (32 MiB) of resident memory per concurrent request. A signature alone does not earn that — anyone can
mint a keypair — so the body waits until the *key* is recognised, unrevoked and bound to an eligible agent user.
Pinned by `TestVerifyCredentialDoesNotTouchTheBody` and `TestVerifyCredentialRejectsBeforeTheBodyOnABadSignature`.

## What the audit table records
Authentication, not authorization and not success. The row is written *before* the handler, because the alternative —
recording afterwards — leaves an agent action unaccounted for whenever the insert fails; here a request that cannot be
recorded is refused instead. Its bare existence therefore implies nothing about what happened next, which is why
`ResponseStatus` exists: the `recordAgentAudit` middleware attaches it, and the repository the router already resolved,
once the handler has returned. A row with 403 is an attempt that was refused. Every method is recorded, reads
included. `RecordAuditOutcome` writes those two columns only, and only while the status is still 0, so the record of
who signed what cannot be altered through it.

`GET …/agent-audit` is gated on `reqAdmin()`, not `reqAnyRepoReader()`: the trail exposes every URL an agent touched,
query strings included, which is more than read access to the repository's contents implies.

## Dependency seam
`github.com/nbd-wtf/go-nostr` is the reference implementation, and importing its root package (which `nip19` also
imports) drags a relay websocket client, `bytedance/sonic` with its runtime-generated assembly, `twitchyliquid64/golang-asm`
and a SQLite event store into the **server** binary — 15 new modules, and MVS bumps to `syndtr/goleveldb` (a direct
dependency: the queue backend) and `modernc.org/sqlite` (the SQLite driver) as collateral. That is a poor trade for a
struct, a deterministic serializer and one BIP-340 call, none of which is close to the project's non-goals ("no
NIP-05/relay publishing"). `modules/nostr` implements exactly that, over `btcsuite/btcd/btcec/v2/schnorr` for BIP-340
— note that `decred/.../secp256k1/v4/schnorr` is *not* usable here: it implements EC-Schnorr-DCRv0, not BIP-340.
Net cost: 4 modules (1 direct), no version bumps to anything pre-existing.

Interoperability is not assumed, it is pinned. `modules/nostr`'s tests carry the NIP-01 worked example's serialization
and id, and a kind-27235 event signed by go-nostr v0.52.3, all produced by that implementation rather than by the code
under test; NIP-19 has three npub/nsec vectors from `go-nostr/nip19`.

## Schema / enum ground truth (verify in-tree before coding — do not assume)
- **Blast radius:** `.terraphim/sync-blast-radius.txt` reserves `routers/api/v1/router.go`, which **does not exist** —
  `func Routes()` lives in `routers/api/v1/api.go`, unreserved. Points 6-7 are therefore permitted, but that entry is a
  dead line: confirm intent on #43 first. The list *does* reserve `.adf-gates.sh` (:87), so this work must not touch
  it — including to skip an environment-sensitive test. The gate subset below is run by hand instead.
- `auth.Method` signature: read services/auth/interface.go; never reproduce from memory. NIP-98's `payload` tag is
  optional when there is no body; confirm against the NIP text. NIP-01's escaping rules are *not* encoding/json's
  defaults (`<`, `>`, `&` must stay verbatim) and getting them wrong changes every event id.

## Acceptance criteria
- A kind-27235 event signed by a registered, unrevoked, scoped key authenticates an API request and appends exactly one
  `agent_audit_event` row, whose `ResponseStatus` is the status the request finally received.
- Bad sig / wrong `u` / wrong `method` / stale `created_at` / body-hash mismatch / revoked key / unregistered key /
  **the same event a second time** each return 401 and write **no** audit row.
- A key scoped `write:issue` can create an issue and is refused (403) at an endpoint outside that scope.
- A NIP-98 credential cannot reach `POST|GET /agent/keys` or `DELETE /agent/keys/{id}`; the owner's own credential can.
- Absent or non-`Nostr` header leaves PAT/OAuth2/Basic behaviour bit-identical.
- `GET …/agent-audit` returns repo-scoped rows, gated on repo admin.
- `GET /agent/audit` returns the owner's rows including the `repo_id = 0` ones the repo endpoint
  cannot show, is scoped to the calling human, and is out of reach of a NIP-98 credential.

## Non-goals
Not replacing or deprecating PATs (strictly additive). No web UI (API only). No NIP-05/relay publishing, no Buzz-side
changes, no audit signing or Merkle chaining. No agent-specific authz model beyond the key's scope — within it, an
agent's permissions are its user's.

## Operational notes
- **The likeliest 401.** The server compares the `u` tag against `setting.AppURL` + the request URI, never against the
  request's own `Host` header (which is attacker-controlled and would let an event signed for one deployment be
  replayed against another sharing a database). A client must therefore sign the URL the *server* believes in: if
  `ROOT_URL` is `https://git.example/` and the agent posts to `http://localhost:3000`, the signature is over the wrong
  string and the request is refused. The reason is logged at Debug. `AppURL`'s **path** counts too: on a sub-path
  install (`ROOT_URL=https://git.example/gitea/`) the router has already stripped `AppSubURL` from `req.URL.Path`
  before authentication runs, so the expected URL is `AppURL` (trailing slash trimmed) + `RequestURI()`, not
  scheme+host+`RequestURI()` — the latter can never match what any client signs.
- **Signed bodies are capped at 32 MiB** (`agentauth.MaxBodySize`). The signature covers a hash of the whole body, so
  the server must buffer it to check; the ceiling is the auth layer's own and is independent of the instance's
  attachment and release limits. Over it the answer is 413, not 401 — the one payload failure whose reason is safe to
  state, since the caller already knows how big its own request was. Larger uploads must use a PAT. Documented in the
  robot's `--help` for the same reason it is here: an invisible ceiling reads as an inexplicable failure.
- **Where non-repository agent activity is readable.** `AuditEvent.RepoID` is 0 for anything that was not
  repository-scoped and for anything refused before `repoAssignment`, and the repo endpoint selects on a non-zero
  `RepoID`. `GET /agent/audit`, owner-scoped, is where those rows are read.
- **The robot signs reads too.** Signing only the mutations would leave the PAT in the environment and on the wire on
  every GET, so anyone who could read either would still hold an unscoped credential — the guarantee would be a
  property of the header, not of the deployment. With `GITEA_NOSTR_KEY` set, `GITEA_TOKEN` is not read and is not
  required to start.

## Test plan
- Unit `modules/nostr`: reference-implementation vectors for serialization, id, signature and bech32; tamper cases.
- Unit `services/agentauth`: table-driven over the rejections + happy path, fixed keypair, frozen clock; the body is
  not read by phase one; `MaxBodySize` binds both when declared and when streamed.
- Unit `services/auth`: `Verify` returns `(nil, nil)` for absent/foreign schemes; scope keys published; verbatim replay
  rejected; reads audited; scopeless key refused.
- Unit `models/agent`: scope mandatory and normalized, `ConsumeEvent` idempotency, prune, outcome update, uniqueness.
- Integration `tests/integration/api_agent_auth_test.go`: accepted mutation, verbatim replay (and that it did not
  happen twice), replay at another URL, tampered body, stale event, unregistered key, scope enforcement,
  self-enrolment/self-revocation refused, key lifecycle, audit gating and refused-request recording.
- Migration re-run is idempotent and the replay guard's unique index really is unique.

## Gates
`make fmt` · `make tidy` · `make lint-go` · `make generate-swagger` (the API gains endpoints and models, and
`swagger-check` is part of `checks-backend`) · `make migrations.sqlite.test` ·
`make test-backend GO_TEST_PACKAGES="code.gitea.io/gitea/modules/nostr/... code.gitea.io/gitea/services/agentauth/... code.gitea.io/gitea/services/auth/... code.gitea.io/gitea/models/agent/... code.gitea.io/gitea/cmd/gitea-robot/..."`.
`.adf-gates.sh` is reserved by the blast-radius list and is **not** edited here; run it unmodified. Go 1.26.0; `2026`
copyright header on every new file.
