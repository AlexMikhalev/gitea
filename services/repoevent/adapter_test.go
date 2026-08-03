// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"strings"
	"testing"

	issues_model "code.gitea.io/gitea/models/issues"
	user_model "code.gitea.io/gitea/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/builder"
)

// A code comment is written to `comment` the moment a reviewer types it, with review_id pointing at
// a review that is still pending. These assertions are about the SQL rather than about rows,
// because the whole point of the condition is that the draft never leaves the database.
func TestPublishedReviewCondShape(t *testing.T) {
	t.Run("anonymous doer has no draft of their own", func(t *testing.T) {
		sql, args, err := builder.ToSQL(publishedReviewCond(nil))
		require.NoError(t, err)

		// A plain comment has review_id 0 and no joined review row: without both of these
		// branches the condition would drop every ordinary comment in the repository.
		assert.Contains(t, sql, "`comment`.review_id=?")
		assert.Contains(t, sql, "`review`.id IS NULL")
		assert.Contains(t, sql, "`review`.type<>?")
		assert.NotContains(t, sql, "reviewer_id")
		assert.Equal(t, []any{0, issues_model.ReviewTypePending}, args)
	})

	t.Run("a signed-in doer also sees their own draft", func(t *testing.T) {
		sql, args, err := builder.ToSQL(publishedReviewCond(&user_model.User{ID: 7}))
		require.NoError(t, err)

		assert.Contains(t, sql, "`review`.reviewer_id=?")
		assert.Equal(t, []any{0, issues_model.ReviewTypePending, int64(7)}, args)
		// The four branches are alternatives, not requirements.
		assert.Equal(t, 3, strings.Count(sql, " OR "))
		assert.NotContains(t, sql, " AND ")
	})
}

func TestSetTextOmitsEmptyValues(t *testing.T) {
	payload := map[string]string{}
	setText(payload, "content", "")
	assert.Empty(t, payload)
}

func TestSetTextKeepsShortValuesVerbatim(t *testing.T) {
	payload := map[string]string{}
	setText(payload, "content", "good work!")

	assert.Equal(t, map[string]string{"content": "good work!"}, payload)
	assert.NotContains(t, payload, "content_truncated",
		"a value that fit must not be marked, or every client sees every event as partial")
}

// comment.content is LONGTEXT and a page is up to MaxLimit rows, so an uncapped payload turns one
// cheap request into a response measured in hundreds of megabytes.
func TestSetTextTruncatesAndMarks(t *testing.T) {
	payload := map[string]string{}
	setText(payload, "content", strings.Repeat("a", maxPayloadTextRunes*2))

	assert.Len(t, payload["content"], maxPayloadTextRunes)
	assert.Equal(t, "true", payload["content_truncated"])
}

// Runes rather than bytes: a cut inside a UTF-8 sequence would put a replacement character into
// the response and make the truncated text differ from the prefix of the original.
func TestSetTextNeverSplitsARune(t *testing.T) {
	payload := map[string]string{}
	original := strings.Repeat("日", maxPayloadTextRunes+10)
	setText(payload, "content", original)

	assert.Equal(t, "true", payload["content_truncated"])
	assert.Equal(t, strings.Repeat("日", maxPayloadTextRunes), payload["content"])
	assert.True(t, strings.HasPrefix(original, payload["content"]))
}
