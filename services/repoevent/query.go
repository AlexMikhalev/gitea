// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"strconv"
	"strings"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/timeutil"

	"xorm.io/builder"
)

// ftsMaxChars bounds the text handed to to_tsvector, and must stay identical to the bound the
// migration 328 index expression uses - a query that does not spell the expression the same way as
// the index does not use it. The bound exists because to_tsvector refuses strings over 1 MB with an
// error, and comment.content is LONGTEXT: an unbounded index expression would turn "someone posted
// a 2 MB comment" into "the INSERT fails".
const ftsMaxChars = 100000

// keysetCond selects the rows of one source that sort strictly after the cursor.
//
// "After" is the stream's order - created_unix DESC, kind ASC, source_id DESC - so which rows of a
// given kind are still ahead depends on how that kind compares to the cursor's kind at an equal
// timestamp:
//
//   - a kind that sorts after the cursor's still owes its rows at the cursor's own second,
//   - the cursor's own kind owes only the rows below it at that second,
//   - a kind that sorts before it has already handed over everything at that second.
//
// Getting this wrong is exactly how a union paginator duplicates or drops rows, so it is decided
// here once rather than in each of the five adapters.
func keysetCond(c *Cursor, kind Kind, createdCol, idCol string) builder.Cond {
	if c == nil {
		return builder.NewCond()
	}
	older := builder.Lt{createdCol: c.CreatedUnix}
	switch strings.Compare(string(kind), string(c.Kind)) {
	case 1: // this kind sorts after the cursor's kind
		return builder.Lte{createdCol: c.CreatedUnix}
	case 0:
		return older.Or(builder.Eq{createdCol: c.CreatedUnix}.And(builder.Lt{idCol: c.SourceID}))
	default:
		return older
	}
}

// rangeCond applies ?since= and ?until=, both inclusive.
//
// Nil is "not given", which is why these are pointers rather than a zero-means-unset timestamp.
// Zero is a real instant a caller can name - `until=1970-01-01T00:00:00Z` asks for the empty
// stream, and answering it with every event in the repository is the opposite of what was asked.
func rangeCond(since, until *timeutil.TimeStamp, createdCol string) builder.Cond {
	cond := builder.NewCond()
	if since != nil {
		cond = cond.And(builder.Gte{createdCol: *since})
	}
	if until != nil {
		cond = cond.And(builder.Lte{createdCol: *until})
	}
	return cond
}

// UsesFullTextSearch reports whether ?q= runs as a Postgres full-text match rather than as a
// case-insensitive LIKE. It follows the same test migration 328 used to decide whether to create
// the GIN indexes, so the two cannot disagree about which path exists.
func UsesFullTextSearch() bool {
	return setting.Database.Type.IsPostgreSQL()
}

// searchCond builds the ?q= filter for one source.
//
// ftsExpr is the indexed expression from migration 328 - it must be written character for character
// as the migration writes it. Passing an empty ftsExpr says this source has no index and always
// takes the LIKE path; that is a per-source decision, not a way to drop the filter. `q` is never
// ignored: on a dialect or a source without an index the same rows come back, they just cost a
// scan instead of an index lookup.
//
// plainto_tsquery rather than to_tsquery, because to_tsquery parses its argument as a tsquery
// expression and raises a syntax error on ordinary prose - `?q=a b` would be a 500 rather than a
// search. plainto_tsquery reads the argument as words, which is what a `q` parameter means, and
// uses the same index.
func searchCond(q, ftsExpr string, likeCols ...string) builder.Cond {
	q = strings.TrimSpace(q)
	if q == "" {
		return builder.NewCond()
	}
	if ftsExpr != "" && UsesFullTextSearch() {
		return builder.Expr(ftsExpr+" @@ plainto_tsquery('english', ?)", q)
	}
	cond := builder.NewCond()
	for _, col := range likeCols {
		cond = cond.Or(db.BuildCaseInsensitiveLike(col, q))
	}
	return cond
}

// TSVectorExpr renders the indexed to_tsvector expression for a single column.
//
// Migration 328 spells the same expression out as a literal instead of calling this, because a
// migration has to keep meaning what it meant on the day it ran and so may not depend on code that
// is still moving. The duplication is deliberate and TestTSVectorExprMatchesMigration328 is what
// keeps the two copies honest.
func TSVectorExpr(col string) string {
	return "to_tsvector('english', left(coalesce(" + col + ", ''), " + strconv.Itoa(ftsMaxChars) + "))"
}

// TSVectorExpr2 is TSVectorExpr over two columns joined by a space, for sources whose searchable
// text is split across a pair of them.
func TSVectorExpr2(a, b string) string {
	return "to_tsvector('english', left(coalesce(" + a + ", '') || ' ' || coalesce(" + b + ", ''), " + strconv.Itoa(ftsMaxChars) + "))"
}
