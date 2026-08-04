// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

// The three write verbs the gitea-automations bridge shells out to
// (crates/bridge/src/robot.rs): `comment`, `edit-issue --add-labels` and `create-pull`.
//
// They live here rather than in the bridge because the bridge must not hold a bearer
// token: gitea-robot carries the NIP-98 agent identity, so every comment, label and pull
// request the bridge produces is attributable to the agent key and revocable with it.
// Every request below therefore goes through setRequestAuth like the read verbs.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"code.gitea.io/gitea/modules/json"
)

// defaultWIPPrefix is the first entry of the shipped
// PULL_REQUEST.WORK_IN_PROGRESS_PREFIXES (modules/setting/repository.go:230). There is no
// `draft` field on CreatePullRequestOption (modules/structs/pull.go:119-144) - Gitea
// decides draft-ness from the title prefix - and the CLI cannot read app.ini, so an
// instance that changed the setting must pass --wip-prefix.
const defaultWIPPrefix = "WIP:"

// apiGetStatus performs a GET and returns the body together with the status code, without
// turning a non-2xx into an error. create-pull needs to tell "no such pull request" (404,
// go ahead and open one) from a real failure; apiGetSafe collapses both into an error.
func apiGetStatus(rawURL string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("error creating request: %v", err)
	}
	if err := setRequestAuth(req, ""); err != nil {
		return "", 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("error reading response: %v", err)
	}
	return string(respBody), resp.StatusCode, nil
}

// usageError marks a failure caused by the flags the caller passed, as opposed to one the
// server returned. Only a usage error is worth answering with the flag list: dumping a wall
// of usage under a 500 buries the line that actually says what went wrong.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func (e usageError) Unwrap() error { return e.err }

// usagef builds a usageError, whose message emitWriteResult follows with fs.Usage().
func usagef(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

// escapeBranchPath escapes a branch name for use in a URL path while keeping its slashes:
// the head of GET /repos/{o}/{r}/pulls/{base}/{head} is matched by a catch-all segment
// (routers/api/v1/api.go:1642), which an escaped %2F would not match.
//
// Only the *head* is a catch-all. {base} is a single segment, so a base branch containing a
// slash cannot be expressed on this route at all - see the check in runCreatePull.
func escapeBranchPath(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// splitLabels turns a comma-separated --add-labels value into label names.
func splitLabels(raw string) []string {
	var out []string
	for name := range strings.SplitSeq(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// commentArgs holds the flags of `comment`.
type commentArgs struct {
	Owner string
	Repo  string
	Issue int64
	Body  string
}

// commentFlagSet declares `comment`'s flags. It is split out so the contract test can
// assert the flag names the bridge emits are the ones declared here.
func commentFlagSet() (*flag.FlagSet, *commentArgs) {
	a := &commentArgs{}
	fs := flag.NewFlagSet("comment", flag.ExitOnError)
	fs.StringVar(&a.Owner, "owner", "", "Repository owner")
	fs.StringVar(&a.Repo, "repo", "", "Repository name")
	fs.Int64Var(&a.Issue, "issue", 0, "Issue index (the #N a human types)")
	fs.StringVar(&a.Body, "body", "", "Comment body")
	return fs, a
}

// runComment posts one issue comment.
func runComment(a commentArgs) (string, error) {
	if a.Owner == "" || a.Repo == "" || a.Issue == 0 {
		return "", usagef("--owner, --repo and --issue required")
	}
	if strings.TrimSpace(a.Body) == "" {
		return "", usagef("--body required")
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/comments", giteaURL, a.Owner, a.Repo, a.Issue)
	data, err := apiPostSafe(endpoint, fmt.Sprintf(`{"body": %s}`, jsonString(a.Body)))
	if err != nil {
		return "", err
	}
	var created struct {
		ID int64 `json:"id"`
	}
	// The comment landed; an unreadable response only costs us the id in the message.
	_ = json.Unmarshal([]byte(data), &created)
	return fmt.Sprintf("✓ Comment posted on %s/%s#%d (comment %d)", a.Owner, a.Repo, a.Issue, created.ID), nil
}

func commentCmd() {
	fs, a := commentFlagSet()
	_ = fs.Parse(os.Args[1:])
	out, err := runComment(*a)
	emitWriteResult(fs, out, err)
}

// editIssueArgs holds the flags of `edit-issue`.
type editIssueArgs struct {
	Owner     string
	Repo      string
	Issue     int64
	AddLabels string
}

// editIssueFlagSet declares `edit-issue`'s flags.
//
// The flag is --add-labels, not --labels, because the operation must be additive: the
// bridge applies status/blocked to issues that already carry labels a human put there.
func editIssueFlagSet() (*flag.FlagSet, *editIssueArgs) {
	a := &editIssueArgs{}
	fs := flag.NewFlagSet("edit-issue", flag.ExitOnError)
	fs.StringVar(&a.Owner, "owner", "", "Repository owner")
	fs.StringVar(&a.Repo, "repo", "", "Repository name")
	fs.Int64Var(&a.Issue, "issue", 0, "Issue index (the #N a human types)")
	fs.StringVar(&a.AddLabels, "add-labels", "", "Comma-separated label names to add, leaving existing labels in place")
	return fs, a
}

// runEditIssue adds labels to an issue without disturbing the ones already on it.
//
// POST .../labels is additive on the server too: newIssueLabels skips labels the issue
// already has (models/issues/issue_label.go:125-130), so re-running this is a no-op rather
// than a duplicate.
func runEditIssue(a editIssueArgs) (string, error) {
	if a.Owner == "" || a.Repo == "" || a.Issue == 0 {
		return "", usagef("--owner, --repo and --issue required")
	}
	labels := splitLabels(a.AddLabels)
	if len(labels) == 0 {
		return "", usagef("--add-labels required (comma-separated label names)")
	}

	payload, err := json.Marshal(map[string]any{"labels": labels})
	if err != nil {
		return "", fmt.Errorf("cannot encode labels: %v", err)
	}
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/labels", giteaURL, a.Owner, a.Repo, a.Issue)
	data, err := apiPostSafe(endpoint, string(payload))
	if err != nil {
		return "", err
	}

	// A label name that does not exist in the repository resolves to no label id and is
	// dropped in silence - GetLabelIDsInRepoByNames returns only what it found
	// (routers/api/v1/repo/issue_label.go:347-360) and the request still answers 200. The
	// response is the issue's whole label set, so the only way to know the label was
	// really applied is to look for it there.
	var applied []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(data), &applied); err != nil {
		return "", fmt.Errorf("cannot parse the label list the server returned: %v", err)
	}
	have := make(map[string]bool, len(applied))
	for _, l := range applied {
		have[strings.ToLower(l.Name)] = true
	}
	var missing []string
	for _, want := range labels {
		if !have[strings.ToLower(want)] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("labels %s are not present on %s/%s#%d after the request; they most likely do not exist in the repository - create them first",
			strings.Join(missing, ", "), a.Owner, a.Repo, a.Issue)
	}

	return fmt.Sprintf("✓ Labels %s added to %s/%s#%d", strings.Join(labels, ", "), a.Owner, a.Repo, a.Issue), nil
}

func editIssueCmd() {
	fs, a := editIssueFlagSet()
	_ = fs.Parse(os.Args[1:])
	out, err := runEditIssue(*a)
	emitWriteResult(fs, out, err)
}

// createPullArgs holds the flags of `create-pull`.
type createPullArgs struct {
	Owner     string
	Repo      string
	Title     string
	Head      string
	Base      string
	Body      string
	Draft     bool
	WIPPrefix string
}

// createPullFlagSet declares `create-pull`'s flags.
func createPullFlagSet() (*flag.FlagSet, *createPullArgs) {
	a := &createPullArgs{}
	fs := flag.NewFlagSet("create-pull", flag.ExitOnError)
	fs.StringVar(&a.Owner, "owner", "", "Repository owner")
	fs.StringVar(&a.Repo, "repo", "", "Repository name")
	fs.StringVar(&a.Title, "title", "", "Pull request title")
	fs.StringVar(&a.Head, "head", "", "Source branch")
	fs.StringVar(&a.Base, "base", "", "Target branch")
	fs.StringVar(&a.Body, "body", "", "Pull request body")
	fs.BoolVar(&a.Draft, "draft", false, "Open the pull request as a draft (prefixes the title with --wip-prefix)")
	fs.StringVar(&a.WIPPrefix, "wip-prefix", defaultWIPPrefix, "Work-in-progress title prefix this instance recognises")
	return fs, a
}

// existingPull is the part of a pull request response the existence probe reads.
//
// State and Merged are both needed. GetPullRequestByBaseHeadInfo has no state predicate
// (models/issues/pull.go:596-601): it matches a closed or merged pull request just as
// happily as an open one, while Gitea's own duplicate check, GetUnmergedPullRequest
// (models/issues/pull.go:502-506), excludes has_merged and is_closed. Since the bridge's
// head branch is deterministic (task/<index>-<slug>), treating that stale row as "already
// open" would report success forever once an issue's first pull request merged, and open
// nothing.
type existingPull struct {
	Number int64  `json:"number"`
	State  string `json:"state"`  // open | closed (modules/structs/pull.go:37)
	Merged bool   `json:"merged"` // HasMerged (modules/structs/pull.go:65)
}

// isOpen reports whether this pull request still blocks opening another one for the same
// base and head.
func (p existingPull) isOpen() bool {
	return !p.Merged && strings.EqualFold(strings.TrimSpace(p.State), "open")
}

// pullNumber pulls the index out of a pull request creation response.
type pullNumber struct {
	Number int64 `json:"number"`
}

// runCreatePull opens a pull request, or reports the one that is already open.
//
// The existence probe is what makes a retry safe. The bridge applies a completed task as
// [open pull, audit comment] and only marks the task once both landed; if the comment
// fails, the next attempt re-runs the whole plan. Gitea refuses a second *unmerged, open*
// pull request for the same base and head, so without this probe that retry would fail
// forever and the audit comment would never be posted.
func runCreatePull(a createPullArgs) (string, error) {
	if a.Owner == "" || a.Repo == "" {
		return "", usagef("--owner and --repo required")
	}
	if a.Title == "" || a.Head == "" || a.Base == "" {
		return "", usagef("--title, --head and --base required")
	}
	// The probe route is GET /repos/{o}/{r}/pulls/{base}/{head} and only {head} is a
	// catch-all (routers/api/v1/api.go:1642). A base of "release/1.0" would be read as
	// base="release", head="1.0/task/...", so the probe would 404, the POST would follow,
	// and Gitea's own duplicate check would reject it - wedging the plan at its first
	// action forever. Refuse loudly instead of probing something that is not this branch.
	if strings.Contains(a.Base, "/") {
		return "", usagef("--base %q contains a slash: the existence probe is GET /repos/{o}/{r}/pulls/{base}/{head}, "+
			"where only {head} is a catch-all segment (routers/api/v1/api.go:1642), so a slashed base cannot be probed "+
			"and this command cannot tell an existing pull request from a missing one", a.Base)
	}

	probe := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%s/%s",
		giteaURL, a.Owner, a.Repo, escapeBranchPath(a.Base), escapeBranchPath(a.Head))
	data, status, err := apiGetStatus(probe)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
		var existing existingPull
		if err := json.Unmarshal([]byte(data), &existing); err != nil {
			return "", fmt.Errorf("a pull request for %s -> %s exists but its response could not be parsed: %v", a.Head, a.Base, err)
		}
		if existing.isOpen() {
			return fmt.Sprintf("Pull request already exists: #%d (%s -> %s)", existing.Number, a.Head, a.Base), nil
		}
		// Closed or merged: it does not stand in the way of a new one, and reporting it as
		// success would mean the caller records a pull request that was never opened.
	case http.StatusNotFound:
		// No pull request for this base and head yet, which is the normal path.
	default:
		return "", fmt.Errorf("error checking for an existing pull request: %s\n%s%s",
			http.StatusText(status), data, authFailureHint(status))
	}

	title := a.Title
	if a.Draft {
		prefix := strings.TrimSpace(a.WIPPrefix)
		if prefix == "" {
			prefix = defaultWIPPrefix
		}
		if !strings.HasPrefix(strings.ToUpper(title), strings.ToUpper(prefix)) {
			title = prefix + " " + title
		}
	}

	payload, err := json.Marshal(map[string]any{
		"title": title,
		"head":  a.Head,
		"base":  a.Base,
		"body":  a.Body,
	})
	if err != nil {
		return "", fmt.Errorf("cannot encode the pull request: %v", err)
	}
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls", giteaURL, a.Owner, a.Repo)
	created, err := apiPostSafe(endpoint, string(payload))
	if err != nil {
		return "", err
	}
	var opened pullNumber
	if err := json.Unmarshal([]byte(created), &opened); err != nil {
		// The pull request exists server-side but its index is unknown; saying so beats
		// reporting "#0". A retry is safe: the probe above finds what was just created.
		return "", fmt.Errorf("pull request opened but its response could not be parsed: %v", err)
	}
	return fmt.Sprintf("✓ Pull request opened: #%d (%s -> %s)", opened.Number, a.Head, a.Base), nil
}

func createPullCmd() {
	fs, a := createPullFlagSet()
	_ = fs.Parse(os.Args[1:])
	out, err := runCreatePull(*a)
	emitWriteResult(fs, out, err)
}

// emitWriteResult prints a write verb's result, or its error, and exits.
//
// The flag list follows only a usageError. A transport failure or a server 500 is not
// answered with a wall of usage: the flags were fine, and burying the one useful line under
// them is how a real failure gets skimmed past in a log.
func emitWriteResult(fs *flag.FlagSet, out string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		var ue usageError
		if errors.As(err, &ue) {
			fs.Usage()
		}
		os.Exit(1)
	}
	fmt.Println(out)
}
