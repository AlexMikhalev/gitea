// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"context"
	"strconv"

	activities_model "code.gitea.io/gitea/models/activities"
	agent_model "code.gitea.io/gitea/models/agent"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	issues_model "code.gitea.io/gitea/models/issues"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"xorm.io/builder"
)

// maxPayloadTextRunes bounds every free-text value an event carries.
//
// comment.content is LONGTEXT, commit_status.description and agent_audit_event.request_url are
// TEXT, and migration 328 reasons explicitly about 2 MB comments existing. A page is up to MaxLimit
// rows, so an uncapped payload turns one cheap request from any repository reader into a
// hundred-megabyte response, repeatably. A stream entry says that something was written; it is not
// the delivery mechanism for the thing itself, and (kind, source_id) is how a client fetches the
// full text from the endpoint that owns it.
//
// Runes rather than bytes, so a cut never lands inside a UTF-8 sequence.
const maxPayloadTextRunes = 4096

// fetchOptions is what every adapter is given. It is the request, not a source's slice of it: each
// adapter decides for itself which parts it can honour and which rows the doer may see.
type fetchOptions struct {
	Repo       *repo_model.Repository
	Doer       *user_model.User
	Permission *access_model.Permission
	// Since and Until are nil when the caller did not give them; see rangeCond.
	Since   *timeutil.TimeStamp
	Until   *timeutil.TimeStamp
	ActorID int64
	Query   string
	Cursor  *Cursor
	Limit   int
}

// adapter reads one source table and projects it into Events.
//
// Each adapter takes its own LIMIT n and the merge keeps the global top n. That is what stops a
// busy source from starving a quiet one: no source is ever read in full, and every source gets to
// offer its n newest rows for the same n slots.
type adapter struct {
	kind  Kind
	fetch func(ctx context.Context, opts *fetchOptions) ([]*Event, error)
}

var adapters = []adapter{
	{KindAction, fetchActions},
	{KindAgentAudit, fetchAgentAudit},
	{KindComment, fetchComments},
	{KindReview, fetchReviews},
	{KindStatus, fetchStatuses},
}

// fetchActions reads `action`.
//
// Visibility is delegated to ActivityQueryCondition rather than reimplemented: it is what the
// dashboard and the repository feed already use, and it encodes three separate rules - the actor's
// own `keep_activity_private` and profile visibility, the set of repositories the doer can reach,
// and the `action.is_private` flag. Reimplementing any of them here would mean this endpoint and
// the feed could disagree about what is private, and only one of them would be right.
//
// It also drops the duplicate rows: `action` holds one row per *receiver*, so a single push is many
// rows. The condition keeps `user_id = act_user_id`, the original, which is why an event appears
// once here rather than once per watcher.
//
// One caveat about `?q=` here, documented on the endpoint too: `action`.content is not prose. For a
// push it is serialized JSON ({"Len":..,"Commits":[..]}), so a search matches against that JSON's
// text - keys and punctuation included - rather than against a message a person wrote. That is the
// column this source has; the alternative would be to not honour `q` for actions at all, which is
// worse than honouring it imprecisely.
func fetchActions(ctx context.Context, opts *fetchOptions) ([]*Event, error) {
	cond, err := activities_model.ActivityQueryCondition(ctx, activities_model.GetFeedsOptions{
		RequestedRepo: opts.Repo,
		Actor:         opts.Doer,
		// The caller already holds read access to this repository, which is what
		// action.is_private guards; ListRepoActivityFeeds passes true here for the same
		// reason. The per-actor and per-repository checks above still apply.
		IncludePrivate: true,
		IncludeDeleted: false,
	})
	if err != nil {
		return nil, err
	}
	cond = cond.And(keysetCond(opts.Cursor, KindAction, "`action`.created_unix", "`action`.id")).
		And(rangeCond(opts.Since, opts.Until, "`action`.created_unix"))
	if opts.ActorID > 0 {
		cond = cond.And(builder.Eq{"`action`.act_user_id": opts.ActorID})
	}
	cond = cond.And(searchCond(opts.Query, TSVectorExpr("`action`.content"), "`action`.content"))

	var rows []*activities_model.Action
	if err := db.GetEngine(ctx).Where(cond).
		OrderBy("`action`.created_unix DESC, `action`.id DESC").
		Limit(opts.Limit).Find(&rows); err != nil {
		return nil, err
	}

	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		payload := map[string]string{"op_type": row.OpType.String()}
		if row.RefName != "" {
			payload["ref_name"] = row.RefName
		}
		setText(payload, "content", row.Content)
		if row.CommentID != 0 {
			payload["comment_id"] = strconv.FormatInt(row.CommentID, 10)
		}
		events = append(events, &Event{
			Kind:        KindAction,
			SourceID:    row.ID,
			ActorID:     row.ActUserID,
			CreatedUnix: row.CreatedUnix,
			RepoID:      row.RepoID,
			Title:       row.OpType.String(),
			Payload:     payload,
		})
	}
	return events, nil
}

// contentBearingCommentTypes are the comment types that carry text a person wrote.
//
// CommentType counts from 0, so the zero value is a real plain comment and not "unset". Everything
// else in that enum - label changes, milestone changes, branch deletions, the whole system-comment
// range - is a record of something already reported by the `action` row next to it, and surfacing
// both would report every state change twice.
//
// CommentTypeReview is in the list but is usually filtered out again by submittedReviewCond, which
// is where the reasoning for that lives.
var contentBearingCommentTypes = []issues_model.CommentType{
	issues_model.CommentTypeComment, // 0
	issues_model.CommentTypeCode,    // 21
	issues_model.CommentTypeReview,  // 22
}

// submittedReviewCond drops the `comment` row Gitea writes beside a review that was submitted.
//
// Submitting a review writes two rows for one action: a `review` row and a CommentTypeReview
// `comment` row carrying the same content, the same poster and the same second
// (models/issues/review.go, SubmitReview; InsertReviews does the same on import). Both sources read
// their own table, so without this the stream reports one submission twice - the exact duplication
// the system comment types are excluded to avoid - and a client cannot even tell that it happened,
// because the two events name each other by nothing.
//
// The `review` row is the copy that is kept. It is the one that carries what the submission was:
// its review type, whether it counted as official, whether it was later dismissed. The comment row
// has none of that, so keeping the comment and dropping the review would lose information rather
// than move it.
//
// It is dropped only when its review is actually there to report it. `review` is LEFT-joined, so a
// CommentTypeReview row whose review was deleted out from under it - or which never had one - joins
// to NULL and stays: it still records something that happened, and vanishing with a row nobody can
// see is worse than a duplicate. Comments of any other type are untouched, which includes
// CommentTypeCode: a line comment is its own remark, not a second copy of the review it belongs to,
// and carries review_id in its payload so a client can group the two.
func submittedReviewCond() builder.Cond {
	return builder.Or(
		builder.Neq{"`comment`.type": issues_model.CommentTypeReview},
		builder.IsNull{"`review`.id"},
	)
}

// fetchComments reads issue and pull request comments.
//
// `comment` has no repo_id; it reaches a repository only through its issue, hence the first join.
// That join is also where visibility comes from: issues and pull requests are separate repository
// units with separate read access, so a doer who can read one and not the other must not see the
// other's comments. Read access to the repository as a whole does not answer this question.
//
// The second join guards the same disclosure fetchReviews does, reached from the other side. A
// CommentTypeCode row is written the moment a reviewer types a line comment, with review_id
// pointing at a review that is still ReviewTypePending - an unsubmitted draft nobody but its author
// may read. models/issues/comment_code.go:99 is where Gitea already draws that line, and
// draftReviewCond mirrors it rather than inventing a second rule that could differ.
func fetchComments(ctx context.Context, opts *fetchOptions) ([]*Event, error) {
	cond, ok := issueScopeCond(opts, "issue")
	if !ok {
		return nil, nil
	}
	cond = cond.And(builder.In("`comment`.type", contentBearingCommentTypes)).
		And(publishedReviewCond(opts.Doer)).
		And(submittedReviewCond()).
		And(keysetCond(opts.Cursor, KindComment, "`comment`.created_unix", "`comment`.id")).
		And(rangeCond(opts.Since, opts.Until, "`comment`.created_unix"))
	if opts.ActorID > 0 {
		cond = cond.And(builder.Eq{"`comment`.poster_id": opts.ActorID})
	}
	cond = cond.And(searchCond(opts.Query, TSVectorExpr("`comment`.content"), "`comment`.content"))

	var rows []*issues_model.Comment
	if err := db.GetEngine(ctx).Table("comment").
		Join("INNER", "issue", "issue.id = `comment`.issue_id").
		Join("LEFT", "review", "`review`.id = `comment`.review_id").
		Select("`comment`.*").
		Where(cond).
		OrderBy("`comment`.created_unix DESC, `comment`.id DESC").
		Limit(opts.Limit).Find(&rows); err != nil {
		return nil, err
	}

	issueIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		issueIDs = append(issueIDs, row.IssueID)
	}
	issues, err := loadIssues(ctx, issueIDs)
	if err != nil {
		return nil, err
	}

	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		payload := map[string]string{
			"comment_type": strconv.Itoa(int(row.Type)),
			"issue_id":     strconv.FormatInt(row.IssueID, 10),
		}
		// The review this comment belongs to, when it belongs to one. A line comment and the
		// review that carries it are two events of one reading, and this is what lets a client
		// put them back together - without it the only thing relating them is a shared second.
		if row.ReviewID != 0 {
			payload["review_id"] = strconv.FormatInt(row.ReviewID, 10)
		}
		setText(payload, "content", row.Content)
		title := ""
		if issue := issues[row.IssueID]; issue != nil {
			title = issue.Title
			payload["index"] = strconv.FormatInt(issue.Index, 10)
			payload["is_pull"] = strconv.FormatBool(issue.IsPull)
		}
		events = append(events, &Event{
			Kind:        KindComment,
			SourceID:    row.ID,
			ActorID:     row.PosterID,
			CreatedUnix: row.CreatedUnix,
			RepoID:      opts.Repo.ID,
			Title:       title,
			Payload:     payload,
		})
	}
	return events, nil
}

// fetchReviews reads published pull request reviews.
//
// ReviewType counts from 0 and ReviewTypePending is that zero: a pending review is a draft the
// reviewer has not submitted, visible to nobody but its author. It is excluded here rather than
// filtered downstream, because a draft leaking into a repository-wide stream is a disclosure, not a
// display bug.
//
// ReviewTypeRequest is excluded for a different reason: it is not a review at all. AddReviewRequest
// writes a `review` row with that type, no content, and reviewer_id set to the person who was
// *asked* (models/issues/review.go) - so reporting it here would announce a review by someone who
// has not reviewed, and Event.ActorID below would make ?actor=<them> match an act somebody else
// performed. The human action is the CommentTypeReviewRequest comment, which this package drops as
// a system comment; the request row is also hard-deleted when the request is withdrawn, so an event
// built from it would disappear from a stream a client had already read.
func fetchReviews(ctx context.Context, opts *fetchOptions) ([]*Event, error) {
	if !opts.Permission.CanRead(unit.TypePullRequests) {
		return nil, nil
	}
	cond := builder.Eq{"issue.repo_id": opts.Repo.ID}.
		And(builder.Eq{"issue.is_pull": true}).
		And(builder.NotIn("`review`.type", issues_model.ReviewTypePending, issues_model.ReviewTypeRequest)).
		And(keysetCond(opts.Cursor, KindReview, "`review`.created_unix", "`review`.id")).
		And(rangeCond(opts.Since, opts.Until, "`review`.created_unix"))
	if opts.ActorID > 0 {
		cond = cond.And(builder.Eq{"`review`.reviewer_id": opts.ActorID})
	}
	cond = cond.And(searchCond(opts.Query, TSVectorExpr("`review`.content"), "`review`.content"))

	var rows []*issues_model.Review
	if err := db.GetEngine(ctx).Table("review").
		Join("INNER", "issue", "issue.id = `review`.issue_id").
		Select("`review`.*").
		Where(cond).
		OrderBy("`review`.created_unix DESC, `review`.id DESC").
		Limit(opts.Limit).Find(&rows); err != nil {
		return nil, err
	}

	issueIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		issueIDs = append(issueIDs, row.IssueID)
	}
	issues, err := loadIssues(ctx, issueIDs)
	if err != nil {
		return nil, err
	}

	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		payload := map[string]string{
			"review_type": strconv.Itoa(int(row.Type)),
			"issue_id":    strconv.FormatInt(row.IssueID, 10),
			"official":    strconv.FormatBool(row.Official),
			"dismissed":   strconv.FormatBool(row.Dismissed),
		}
		setText(payload, "content", row.Content)
		if row.CommitID != "" {
			payload["commit_id"] = row.CommitID
		}
		title := ""
		if issue := issues[row.IssueID]; issue != nil {
			title = issue.Title
			payload["index"] = strconv.FormatInt(issue.Index, 10)
		}
		events = append(events, &Event{
			Kind:        KindReview,
			SourceID:    row.ID,
			ActorID:     row.ReviewerID,
			CreatedUnix: row.CreatedUnix,
			RepoID:      opts.Repo.ID,
			Title:       title,
			Payload:     payload,
		})
	}
	return events, nil
}

// fetchStatuses reads commit statuses.
//
// CommitStatusState is a string enum, not an int one, so the state travels into the payload as
// itself rather than as a number whose meaning depends on a constant block somewhere else.
func fetchStatuses(ctx context.Context, opts *fetchOptions) ([]*Event, error) {
	if !opts.Permission.CanRead(unit.TypeCode) {
		return nil, nil
	}
	cond := builder.Eq{"`commit_status`.repo_id": opts.Repo.ID}.
		And(keysetCond(opts.Cursor, KindStatus, "`commit_status`.created_unix", "`commit_status`.id")).
		And(rangeCond(opts.Since, opts.Until, "`commit_status`.created_unix"))
	if opts.ActorID > 0 {
		cond = cond.And(builder.Eq{"`commit_status`.creator_id": opts.ActorID})
	}
	cond = cond.And(searchCond(opts.Query,
		TSVectorExpr2("`commit_status`.description", "`commit_status`.context"),
		"`commit_status`.description", "`commit_status`.context"))

	var rows []*git_model.CommitStatus
	if err := db.GetEngine(ctx).Where(cond).
		OrderBy("`commit_status`.created_unix DESC, `commit_status`.id DESC").
		Limit(opts.Limit).Find(&rows); err != nil {
		return nil, err
	}

	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		payload := map[string]string{
			"state": string(row.State),
			"sha":   row.SHA,
		}
		setText(payload, "context", row.Context)
		setText(payload, "target_url", row.TargetURL)
		events = append(events, &Event{
			Kind:        KindStatus,
			SourceID:    row.ID,
			ActorID:     row.CreatorID,
			CreatedUnix: row.CreatedUnix,
			RepoID:      row.RepoID,
			// description, context and target_url are all TEXT columns a CI system fills in.
			Title:   truncateText(row.Description),
			Payload: payload,
		})
	}
	return events, nil
}

// fetchAgentAudit reads the NIP-98 audit trail.
//
// Repository admin only, the same bar GET /repos/{owner}/{repo}/agent-audit sets, and for the same
// reason: the trail records every URL an agent touched, query strings included, which is more than
// read access to the repository's contents implies. A reader who is not an admin gets a stream
// without this kind rather than a 403, because the answer to "what may I see" is the stream itself.
func fetchAgentAudit(ctx context.Context, opts *fetchOptions) ([]*Event, error) {
	if opts.Permission == nil || !opts.Permission.IsAdmin() {
		return nil, nil
	}
	cond := keysetCond(opts.Cursor, KindAgentAudit, "`agent_audit_event`.created_unix", "`agent_audit_event`.id").
		And(rangeCond(opts.Since, opts.Until, "`agent_audit_event`.created_unix"))
	if opts.ActorID > 0 {
		cond = cond.And(builder.Eq{"`agent_audit_event`.agent_user_id": opts.ActorID})
	}
	// No FTS index on request_url: a URL is not English prose, and to_tsvector chops it into
	// tokens that make "search for a path" miss. The LIKE path is the better answer here, not a
	// degraded one, so `q` is honoured on every dialect by the same query.
	cond = cond.And(searchCond(opts.Query, "", "`agent_audit_event`.request_url"))

	rows, err := agent_model.FindRepoAuditEventsAfter(ctx, opts.Repo.ID, cond, opts.Limit)
	if err != nil {
		return nil, err
	}

	events := make([]*Event, 0, len(rows))
	for _, row := range rows {
		payload := map[string]string{
			"method":   row.Method,
			"event_id": row.EventID,
			"pub_key":  row.PubKey,
		}
		// request_url is TEXT and a signed URL may carry an arbitrarily long query string.
		setText(payload, "request_url", row.RequestURL)
		if row.ResponseStatus != 0 {
			payload["response_status"] = strconv.Itoa(row.ResponseStatus)
		}
		events = append(events, &Event{
			Kind:        KindAgentAudit,
			SourceID:    row.ID,
			ActorID:     row.AgentUserID,
			CreatedUnix: row.CreatedUnix,
			RepoID:      row.RepoID,
			Title:       truncateText(row.Method + " " + row.RequestURL),
			Payload:     payload,
		})
	}
	return events, nil
}

// publishedReviewCond keeps the comments of published reviews, and the doer's own drafts.
//
// The branches are the three ways a row is legitimately visible, spelled out rather than written as
// a NOT: `review` is LEFT-joined, so a plain comment's review columns are NULL, and
// NOT(NULL = pending) is NULL, which is not true. Only an explicit "there is no review" branch
// keeps those rows, and getting that wrong empties the stream of every ordinary comment.
//
// An anonymous doer gets no fourth branch, because there is no draft that could be theirs. Site
// admins get none either: comment_code.go compares against the reviewer id alone, and an
// unsubmitted draft is not something admin over the repository entitles anyone to read.
func publishedReviewCond(doer *user_model.User) builder.Cond {
	cond := builder.Or(
		builder.Eq{"`comment`.review_id": 0},
		builder.IsNull{"`review`.id"},
		builder.Neq{"`review`.type": issues_model.ReviewTypePending},
	)
	if doer != nil {
		cond = cond.Or(builder.Eq{"`review`.reviewer_id": doer.ID})
	}
	return cond
}

// truncateText bounds one free-text value at maxPayloadTextRunes.
func truncateText(s string) string {
	return util.TruncateRunes(s, maxPayloadTextRunes)
}

// setText stores a free-text payload value, dropping it when empty and marking it when it did not
// fit. The `<key>_truncated` marker is what lets a client tell "this is all of it" from "this is
// the beginning of it" - without it a truncated comment reads as a complete, shorter comment.
func setText(payload map[string]string, key, value string) {
	if value == "" {
		return
	}
	if cut := truncateText(value); cut != value {
		value = cut
		payload[key+"_truncated"] = "true"
	}
	payload[key] = value
}

// issueScopeCond restricts a join on `issue` to the rows of this repository the doer may read, and
// reports false when they may read neither issues nor pull requests - in which case the caller has
// no query to run at all, rather than a query that returns nothing.
func issueScopeCond(opts *fetchOptions, table string) (builder.Cond, bool) {
	canIssues := opts.Permission.CanRead(unit.TypeIssues)
	canPulls := opts.Permission.CanRead(unit.TypePullRequests)
	if !canIssues && !canPulls {
		return nil, false
	}
	cond := builder.NewCond().And(builder.Eq{table + ".repo_id": opts.Repo.ID})
	if !canIssues {
		cond = cond.And(builder.Eq{table + ".is_pull": true})
	}
	if !canPulls {
		cond = cond.And(builder.Eq{table + ".is_pull": false})
	}
	return cond, true
}

// loadIssues fetches the issues a page of comments or reviews belongs to, in one query, so that an
// event can carry its issue's title and index without the adapter running a query per row.
func loadIssues(ctx context.Context, ids []int64) (map[int64]*issues_model.Issue, error) {
	result := make(map[int64]*issues_model.Issue, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var issues []*issues_model.Issue
	if err := db.GetEngine(ctx).In("id", ids).Find(&issues); err != nil {
		return nil, err
	}
	for _, issue := range issues {
		result[issue.ID] = issue
	}
	return result, nil
}
