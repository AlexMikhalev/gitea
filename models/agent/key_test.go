// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"strings"
	"testing"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/unittest"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A NIP-19 test vector pair from the NIP-19 spec.
const (
	specPubKeyHex = "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"
	specNpub      = "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6"
)

// Any valid scope will do for the tests that are not about scopes; what matters is that one is
// present, because RegisterKey refuses a key without.
const testScope = auth_model.AccessTokenScopeWriteIssue

// An arbitrary secret key, used to mint real signatures below. It is not the counterpart of
// specPubKeyHex: the audit rows have to be signed by *something* whose signature verifies, and
// the NIP-19 vector above is a public key with no published secret.
const testSecretKeyHex = "1111111111111111111111111111111111111111111111111111111111111111"

// signedAuditEvent mints a genuinely signed NIP-98 event and returns it projected onto an audit
// row, the way services/auth does for a real request.
//
// The tests build rows this way rather than from made-up strings because InsertAuditEvent now
// refuses a row whose stored fields do not re-derive its event id - which is the property that
// makes the trail checkable, and would be untested if every fixture sidestepped it. nonce is
// varied per call for the same reason a real client varies it: without it two rows built in the
// same second are the same event.
func signedAuditEvent(t *testing.T, keyID, repoID int64, nonce, requestURL string) *AuditEvent {
	t.Helper()

	event := &nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      27235,
		Tags: nostr.Tags{
			{"u", requestURL},
			{"method", "POST"},
			{"nonce", nonce},
		},
	}
	require.NoError(t, event.Sign(testSecretKeyHex))

	tags, err := json.Marshal(event.Tags)
	require.NoError(t, err)

	return &AuditEvent{
		RepoID:           repoID,
		AgentUserID:      2,
		OwnerUserID:      1,
		AgentKeyID:       keyID,
		EventID:          event.ID,
		PubKey:           event.PubKey,
		Method:           "POST",
		RequestURL:       requestURL,
		EventCreatedUnix: timeutil.TimeStamp(event.CreatedAt),
		EventKind:        event.Kind,
		Nonce:            nonce,
		EventTags:        string(tags),
		EventContent:     event.Content,
		Sig:              event.Sig,
	}
}

func TestPubKeyFromInput(t *testing.T) {
	t.Run("hex in, npub derived", func(t *testing.T) {
		pubKey, npub, err := PubKeyFromInput(specPubKeyHex)
		require.NoError(t, err)
		assert.Equal(t, specPubKeyHex, pubKey)
		assert.Equal(t, specNpub, npub)
	})

	t.Run("npub in, hex derived", func(t *testing.T) {
		pubKey, npub, err := PubKeyFromInput(specNpub)
		require.NoError(t, err)
		assert.Equal(t, specPubKeyHex, pubKey)
		assert.Equal(t, specNpub, npub)
	})

	t.Run("upper case hex is normalized", func(t *testing.T) {
		pubKey, _, err := PubKeyFromInput(strings.ToUpper(specPubKeyHex))
		require.NoError(t, err)
		assert.Equal(t, specPubKeyHex, pubKey)
	})

	for _, bad := range []string{
		"",
		"deadbeef",                      // too short
		specPubKeyHex + "00",            // too long
		strings.Repeat("z", 64),         // not hex
		"npub1thisisnotavalidnpubatall", // bad checksum
	} {
		t.Run("rejects "+bad, func(t *testing.T) {
			_, _, err := PubKeyFromInput(bad)
			assert.Error(t, err)
		})
	}
}

func TestRegisterAndLookupKey(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: strings.ToUpper(specPubKeyHex), Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))
	assert.NotZero(t, key.ID)
	assert.Equal(t, specPubKeyHex, key.PubKey, "the stored key must be normalized to lower-case hex")
	assert.Equal(t, specNpub, key.Npub, "npub must be filled in when the caller omits it")

	got, err := GetKeyByPubKey(ctx, specPubKeyHex)
	require.NoError(t, err)
	assert.Equal(t, key.ID, got.ID)
	assert.False(t, got.IsRevoked())

	// The lookup is case-insensitive because callers pass whatever the event carried.
	got, err = GetKeyByPubKey(ctx, strings.ToUpper(specPubKeyHex))
	require.NoError(t, err)
	assert.Equal(t, key.ID, got.ID)

	_, err = GetKeyByPubKey(ctx, strings.Repeat("a", 64))
	assert.True(t, IsErrAgentKeyNotExist(err), "got %v", err)

	keys, err := ListKeysVisibleToUser(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, keys, 1)

	// The agent the key authenticates as can see it too, even though it does not own it: an
	// admin-registered key is owned by the admin, and the account it signs for has to be able
	// to find out that it exists.
	keys, err = ListKeysVisibleToUser(ctx, 2)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, key.ID, keys[0].ID)

	// Nobody else sees it.
	keys, err = ListKeysVisibleToUser(ctx, 3)
	require.NoError(t, err)
	assert.Empty(t, keys)

	// Revocation is a flag, never a delete: the audit rows must keep pointing at a live row.
	require.NoError(t, RevokeKey(ctx, key.ID))
	got, err = GetKeyByPubKey(ctx, specPubKeyHex)
	require.NoError(t, err)
	assert.True(t, got.IsRevoked())

	revokedAt := got.RevokedUnix
	require.NoError(t, RevokeKey(ctx, key.ID))
	got, err = GetKeyByPubKey(ctx, specPubKeyHex)
	require.NoError(t, err)
	assert.Equal(t, revokedAt, got.RevokedUnix, "re-revoking must not move the timestamp")
}

func TestRegisterKeyRejectsIncompleteOwnership(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	assert.Error(t, RegisterKey(ctx, &Key{OwnerUserID: 0, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}))
	assert.Error(t, RegisterKey(ctx, &Key{OwnerUserID: 1, AgentUserID: 0, PubKey: specPubKeyHex, Scope: testScope}))
	assert.Error(t, RegisterKey(ctx, &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: "nonsense", Scope: testScope}))
}

func TestAuditEventsAreRepoScoped(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))

	for i, repoID := range []int64{1, 1, 2} {
		require.NoError(t, InsertAuditEvent(ctx, signedAuditEvent(t, key.ID, repoID,
			fmt.Sprintf("scoped-%d", i), "https://gitea.example.com/api/v1/repos/x/y/issues")))
	}

	events, total, err := FindAuditEvents(ctx, FindAuditEventsOptions{ListOptions: db.ListOptionsAll, RepoID: 1})
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	require.Len(t, events, 2)
	assert.Greater(t, events[0].ID, events[1].ID, "newest row first")
	for _, event := range events {
		assert.EqualValues(t, 1, event.RepoID)
	}

	_, total, err = FindAuditEvents(ctx, FindAuditEventsOptions{ListOptions: db.ListOptionsAll, RepoID: 2})
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)

	// A repo with no agent traffic must not see anyone else's rows.
	_, total, err = FindAuditEvents(ctx, FindAuditEventsOptions{ListOptions: db.ListOptionsAll, RepoID: 999})
	require.NoError(t, err)
	assert.EqualValues(t, 0, total)
}

// A key with no scope is the one shape that must never reach the database: services/auth would
// present it to routers/api/v1 as "no scope to check", and every scope guard in the API would
// wave the request through.
func TestRegisterKeyRequiresAScope(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	err := RegisterKey(ctx, &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex})
	require.Error(t, err)
	assert.ErrorIs(t, err, util.ErrInvalidArgument)

	assert.Error(t, RegisterKey(ctx, &Key{
		OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: "not-a-real-scope",
	}))

	// A valid scope is stored normalized, so that HasScope sees the canonical form.
	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: "write:issue,read:issue"}
	require.NoError(t, RegisterKey(ctx, key))
	assert.Equal(t, auth_model.AccessTokenScopeWriteIssue, key.Scope)
}

func TestGetKeyByID(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))

	got, err := GetKeyByID(ctx, key.ID)
	require.NoError(t, err)
	assert.Equal(t, key.PubKey, got.PubKey)

	_, err = GetKeyByID(ctx, key.ID+10000)
	assert.True(t, IsErrAgentKeyNotExist(err), "got %v", err)
}

// The replay guard is the reason a captured header cannot simply be re-sent.
func TestConsumeEvent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	const eventID = "1111111111111111111111111111111111111111111111111111111111111111"
	expires := timeutil.TimeStampNow() + 60

	require.NoError(t, ConsumeEvent(ctx, eventID, expires))

	err := ConsumeEvent(ctx, eventID, expires)
	require.Error(t, err)
	assert.True(t, IsErrEventReplayed(err), "got %v", err)
	assert.ErrorIs(t, err, util.ErrAlreadyExist)

	// A different event is unaffected: the guard is per-event, not a global lock.
	require.NoError(t, ConsumeEvent(ctx, strings.Repeat("2", 64), expires))

	assert.Error(t, ConsumeEvent(ctx, "", expires))
}

// Spent ids are worthless once the event could no longer be accepted anyway, and the table would
// otherwise be the one part of this feature that grows without bound.
func TestPruneUsedEvents(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	now := timeutil.TimeStampNow()
	require.NoError(t, ConsumeEvent(ctx, strings.Repeat("3", 64), now-120))
	require.NoError(t, ConsumeEvent(ctx, strings.Repeat("4", 64), now+120))

	removed, err := PruneUsedEvents(ctx, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed)

	// The pruned id can be spent again - which is safe, because the event carrying it is now
	// too stale for the freshness check to accept in the first place.
	require.NoError(t, ConsumeEvent(ctx, strings.Repeat("3", 64), now+120))

	// The unexpired one is still guarded.
	assert.True(t, IsErrEventReplayed(ConsumeEvent(ctx, strings.Repeat("4", 64), now+120)))
}

// The outcome is attached after the handler has run; nothing else about the row may move, and it
// may not be attached twice.
func TestRecordAuditOutcome(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))

	event := signedAuditEvent(t, key.ID, 0, "outcome", "https://gitea.example.com/api/v1/repos/x/y/issues")
	require.NoError(t, InsertAuditEvent(ctx, event))
	assert.Zero(t, event.ResponseStatus)

	require.NoError(t, RecordAuditOutcome(ctx, event.ID, 1, 403))

	stored := new(AuditEvent)
	has, err := db.GetEngine(ctx).ID(event.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, 403, stored.ResponseStatus)
	assert.EqualValues(t, 1, stored.RepoID)
	assert.Equal(t, event.EventID, stored.EventID, "the identifying columns must not move")
	assert.Equal(t, "POST", stored.Method)

	// A second call cannot rewrite an outcome that has already been observed.
	require.NoError(t, RecordAuditOutcome(ctx, event.ID, 2, 201))
	has, err = db.GetEngine(ctx).ID(event.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, 403, stored.ResponseStatus)
	assert.EqualValues(t, 1, stored.RepoID)

	assert.Error(t, RecordAuditOutcome(ctx, 0, 1, 200))
}

// Two rows carrying one event id would mean the replay guard had been bypassed, so the database
// refuses them outright rather than trusting the code above it.
func TestAuditEventIDIsUnique(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))

	// The same nonce and the same second give the same event id, which is exactly the collision
	// the unique index has to refuse.
	first := signedAuditEvent(t, key.ID, 0, "duplicate", "https://gitea.example.com/api/v1/repos/x/y/issues")
	second := *first
	require.NoError(t, InsertAuditEvent(ctx, first))
	assert.Error(t, InsertAuditEvent(ctx, &second))
}

// The trail's central claim is that a row can be checked against itself. Both halves are asserted
// here: a row as written verifies, and the same row with its recorded method or URL edited - the
// two columns a reader actually trusts - does not.
func TestAuditEventVerifiesAgainstItsOwnSignature(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))

	const url = "https://gitea.example.com/api/v1/repos/x/y/issues"
	event := signedAuditEvent(t, key.ID, 1, "verifiable", url)
	require.NoError(t, InsertAuditEvent(ctx, event))

	stored := new(AuditEvent)
	has, err := db.GetEngine(ctx).ID(event.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	require.NoError(t, stored.VerifyEvent(), "a row must verify as read back out of the database")

	tampered := *stored
	tampered.Method = "GET"
	assert.Error(t, tampered.VerifyEvent(), "an edited method must not verify")

	tampered = *stored
	tampered.RequestURL = "https://gitea.example.com/api/v1/repos/x/y/harmless"
	assert.Error(t, tampered.VerifyEvent(), "an edited url must not verify")

	tampered = *stored
	tampered.Sig = strings.Repeat("0", len(tampered.Sig))
	assert.Error(t, tampered.VerifyEvent(), "a row whose signature was replaced must not verify")

	// The URL comparison is on the normalized form, not byte-for-byte, so a row storing the
	// normalized URL for a tag that was not already normalized still verifies.
	equivalent := *stored
	equivalent.RequestURL = url
	assert.NoError(t, equivalent.VerifyEvent())
}

// InsertAuditEvent refuses a projection that cannot re-derive its own event id. Without this the
// property above could rot silently: every row would still look complete and none would verify.
func TestInsertAuditEventRejectsAnUnverifiableProjection(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	key := &Key{OwnerUserID: 1, AgentUserID: 2, PubKey: specPubKeyHex, Scope: testScope}
	require.NoError(t, RegisterKey(ctx, key))

	dropped := signedAuditEvent(t, key.ID, 0, "lossy", "https://gitea.example.com/api/v1/repos/x/y/issues")
	dropped.EventTags = ""
	assert.Error(t, InsertAuditEvent(ctx, dropped), "a row with its tags dropped cannot re-derive its id")

	unsigned := signedAuditEvent(t, key.ID, 0, "unsigned", "https://gitea.example.com/api/v1/repos/x/y/issues")
	unsigned.Sig = ""
	assert.Error(t, InsertAuditEvent(ctx, unsigned), "a row with no signature records a claim, not evidence")
}

func TestInsertAuditEventRejectsIncompleteAttribution(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := t.Context()

	assert.Error(t, InsertAuditEvent(ctx, &AuditEvent{AgentUserID: 0, AgentKeyID: 1, EventID: "a"}))
	assert.Error(t, InsertAuditEvent(ctx, &AuditEvent{AgentUserID: 1, AgentKeyID: 0, EventID: "a"}))
	assert.Error(t, InsertAuditEvent(ctx, &AuditEvent{AgentUserID: 1, AgentKeyID: 1}),
		"a row with no event id could not be tied back to anything the agent signed")
}
