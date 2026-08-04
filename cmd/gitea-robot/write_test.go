// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// bridgeArgv is the argv the gitea-automations bridge builds for each write verb
// (crates/bridge/src/robot.rs: comment_args, add_labels_args, create_pull_args).
//
// The bridge's own tests assert only that it agrees with itself about these strings. This
// table is the other half of that contract: it is what makes "the verb exists and takes
// these flags" a checked fact rather than a belief. Changing either side without the other
// fails TestBridgeWriteVerbsExist below.
var bridgeArgv = map[string]struct {
	flagSet func() *flag.FlagSet
	emitted []string
}{
	"comment": {
		flagSet: func() *flag.FlagSet { fs, _ := commentFlagSet(); return fs },
		emitted: []string{"owner", "repo", "issue", "body"},
	},
	"edit-issue": {
		flagSet: func() *flag.FlagSet { fs, _ := editIssueFlagSet(); return fs },
		emitted: []string{"owner", "repo", "issue", "add-labels"},
	},
	"create-pull": {
		flagSet: func() *flag.FlagSet { fs, _ := createPullFlagSet(); return fs },
		// wip-prefix travels with draft and never without it (crates/bridge/src/robot.rs:133-137):
		// Gitea has no draft field on a pull request, so draft-ness *is* the title prefix, and
		// an instance that changed PULL_REQUEST.WORK_IN_PROGRESS_PREFIXES needs its own. Drop it
		// from this flag set and every `completed` event becomes "flag provided but not
		// defined" and an exit 1, with the bridge's own tests still green.
		emitted: []string{"owner", "repo", "title", "head", "base", "body", "draft", "wip-prefix"},
	},
}

// TestBridgeWriteVerbsExist is the check that would have caught the whole outbound leg
// calling subcommands that did not exist: every verb the bridge shells out to must be in
// the dispatch table, and must declare every flag the bridge passes.
func TestBridgeWriteVerbsExist(t *testing.T) {
	for verb, contract := range bridgeArgv {
		if _, ok := commands[verb]; !ok {
			t.Errorf("gitea-robot has no %q command, but the bridge shells out to it", verb)
			continue
		}
		fs := contract.flagSet()
		for _, name := range contract.emitted {
			if fs.Lookup(name) == nil {
				t.Errorf("%s does not declare --%s, which the bridge passes", verb, name)
			}
		}
	}
}

// bridgePreflightVerb is crates/bridge/src/robot.rs's PREFLIGHT_VERB.
//
// The bridge spawns this binary once at startup with that argument to check its write leg
// before any terminal event needs it. The probe works precisely because the verb does not
// exist: main() validates the credential first and only then refuses to dispatch, so the two
// answers are distinguishable and neither makes a request.
const bridgePreflightVerb = "gitea-automations-preflight"

// TestBridgePreflightProbeStaysDistinguishable guards the three facts that probe rests on.
//
// Make this a real command and the preflight starts making a request and reading its result as
// a verdict about the credential. Reword either message and the preflight stops recognising the
// answer it gets: a missing credential then reports as "cannot verify" instead of "will not
// write", which is a green-enough `check` in front of a daemon whose every comment, label and
// pull request fails at action 0 - the escalation comment included.
func TestBridgePreflightProbeStaysDistinguishable(t *testing.T) {
	if _, ok := commands[bridgePreflightVerb]; ok {
		t.Errorf("%q is a dispatchable command; the bridge's write-leg preflight relies on it not being one", bridgePreflightVerb)
	}
	// The exact substrings crates/bridge/src/robot.rs (classify_preflight) matches on.
	if !strings.Contains(missingCredentialError, "environment variable required") {
		t.Errorf("missingCredentialError = %q, want it to contain \"environment variable required\"", missingCredentialError)
	}
	if !strings.Contains(unknownCommandError, "Unknown command") {
		t.Errorf("unknownCommandError = %q, want it to contain \"Unknown command\"", unknownCommandError)
	}
	// …and they must stay two different answers, or the probe cannot tell them apart at all.
	if strings.Contains(missingCredentialError, "Unknown command") {
		t.Errorf("the two refusals must not overlap: %q", missingCredentialError)
	}
}

// documentedCommands extracts the verbs listed under "Commands:" in printUsage().
//
// A substring search over the whole usage text is not enough in either direction: "comment"
// occurs inside the create-pull description, so a verb could look documented purely because
// another line mentions it in prose, and a verb dropped from the dispatch table but left in
// the list would go unnoticed. Reading the block gives an exact set to compare.
func documentedCommands(t *testing.T) map[string]bool {
	t.Helper()
	usage, err := captureStdout(printUsage)
	if err != nil {
		t.Fatalf("captureStdout() = %v", err)
	}
	out := map[string]bool{}
	inBlock := false
	for line := range strings.SplitSeq(usage, "\n") {
		if strings.TrimSpace(line) == "Commands:" {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		// The block ends at the first line that is not an indented entry.
		if strings.TrimSpace(line) == "" || !strings.HasPrefix(line, " ") {
			break
		}
		out[strings.Fields(line)[0]] = true
	}
	if len(out) == 0 {
		t.Fatal("printUsage() has no Commands: block to check against")
	}
	return out
}

// TestEveryDocumentedCommandDispatches guards the usage text against the dispatch table, in
// both directions: an undocumented verb is invisible, and a documented one that no longer
// dispatches is a promise the CLI does not keep.
func TestEveryDocumentedCommandDispatches(t *testing.T) {
	documented := documentedCommands(t)
	for verb := range commands {
		if !documented[verb] {
			t.Errorf("command %q is dispatchable but undocumented in printUsage()", verb)
		}
	}
	for verb := range documented {
		if _, ok := commands[verb]; !ok {
			t.Errorf("printUsage() documents %q, but it is not in the dispatch table", verb)
		}
	}
}

// writeTestServer records the requests a write verb makes and answers them from handler.
func writeTestServer(t *testing.T, requests *[]recordedRequest, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*requests = append(*requests, recordedRequest{r.Method, r.URL.Path, string(body)})
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	withGiteaURL(t, server.URL)
}

func TestCommentPostsToTheIssue(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/o/r/issues/57/comments" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 4242}`))
	})

	out, err := runComment(commentArgs{Owner: "o", Repo: "r", Issue: 57, Body: "audit note"})
	if err != nil {
		t.Fatalf("runComment() = %v", err)
	}
	if !strings.Contains(out, "4242") {
		t.Errorf("runComment() = %q, want the comment id", out)
	}
	if len(requests) != 1 || !strings.Contains(requests[0].Body, `"audit note"`) {
		t.Errorf("requests = %+v", requests)
	}
}

func TestCommentRequiresIssueAndBody(t *testing.T) {
	for name, args := range map[string]commentArgs{
		"no issue": {Owner: "o", Repo: "r", Body: "x"},
		"no body":  {Owner: "o", Repo: "r", Issue: 1},
		"blank":    {Owner: "o", Repo: "r", Issue: 1, Body: "   "},
		"no owner": {Repo: "r", Issue: 1, Body: "x"},
	} {
		if _, err := runComment(args); err == nil {
			t.Errorf("runComment(%s) accepted incomplete arguments", name)
		}
	}
}

func TestEditIssueAddsLabelsAdditively(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		// POST (add), never PUT (replace): a human's labels must survive.
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/o/r/issues/57/labels" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"kind/bug"},{"name":"status/blocked"}]`))
	})

	out, err := runEditIssue(editIssueArgs{Owner: "o", Repo: "r", Issue: 57, AddLabels: "status/blocked"})
	if err != nil {
		t.Fatalf("runEditIssue() = %v", err)
	}
	if !strings.Contains(out, "status/blocked") {
		t.Errorf("runEditIssue() = %q", out)
	}
	if len(requests) != 1 || !strings.Contains(requests[0].Body, `["status/blocked"]`) {
		t.Errorf("requests = %+v", requests)
	}
}

// TestEditIssueDetectsASilentlyDroppedLabel covers the trap in
// routers/api/v1/repo/issue_label.go:347-360: a label name that does not exist in the
// repository resolves to no id, is dropped, and the request still answers 200.
func TestEditIssueDetectsASilentlyDroppedLabel(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"kind/bug"}]`))
	})

	_, err := runEditIssue(editIssueArgs{Owner: "o", Repo: "r", Issue: 57, AddLabels: "status/blocked"})
	if err == nil {
		t.Fatal("runEditIssue() accepted a 200 that did not apply the label")
	}
	if !strings.Contains(err.Error(), "status/blocked") {
		t.Errorf("runEditIssue() = %v, want the missing label named", err)
	}
}

func TestEditIssueRequiresLabels(t *testing.T) {
	if _, err := runEditIssue(editIssueArgs{Owner: "o", Repo: "r", Issue: 1, AddLabels: " , "}); err == nil {
		t.Error("runEditIssue() accepted an empty label list")
	}
}

func TestCreatePullOpensAPullRequest(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/pulls/main/task/57-daemon":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/o/r/pulls":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 63}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	})

	out, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "issue #57: daemon",
		Head: "task/57-daemon", Base: "main", Body: "Refs #57",
	})
	if err != nil {
		t.Fatalf("runCreatePull() = %v", err)
	}
	if !strings.Contains(out, "#63") {
		t.Errorf("runCreatePull() = %q", out)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %+v, want a probe then a create", requests)
	}
	if !strings.Contains(requests[1].Body, `"Refs #57"`) {
		t.Errorf("create body = %q", requests[1].Body)
	}
	if strings.Contains(requests[1].Body, "WIP") {
		t.Errorf("create body = %q, want no WIP prefix without --draft", requests[1].Body)
	}
}

// TestCreatePullIsIdempotent is what makes the bridge's retry safe: the audit comment can
// fail after the pull request landed, and the next attempt re-runs the whole plan. Gitea
// refuses a second pull request for the same base and head, so a create-pull that did not
// check first would wedge that task forever.
func TestCreatePullIsIdempotent(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("create-pull posted despite an existing pull request")
		}
		_, _ = w.Write([]byte(`{"number": 63, "state": "open", "merged": false}`))
	})

	out, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "issue #57: daemon", Head: "task/57-daemon", Base: "main",
	})
	if err != nil {
		t.Fatalf("runCreatePull() = %v", err)
	}
	if !strings.Contains(out, "already exists") || !strings.Contains(out, "#63") {
		t.Errorf("runCreatePull() = %q, want the existing pull request reported as success", out)
	}
	if len(requests) != 1 {
		t.Errorf("requests = %+v, want the probe only", requests)
	}
}

// TestCreatePullIgnoresAMergedOrClosedPullRequest covers the trap in
// GetPullRequestByBaseHeadInfo (models/issues/pull.go:596-601): it has no state predicate,
// so it matches a merged or closed pull request too, while Gitea's own duplicate check
// GetUnmergedPullRequest (:502-506) excludes both. The bridge's head branch is
// deterministic, so without this the second completed run for an issue whose first pull
// request already merged would report "already exists" as success and open nothing.
func TestCreatePullIgnoresAMergedOrClosedPullRequest(t *testing.T) {
	for name, probeBody := range map[string]string{
		"merged": `{"number": 12, "state": "closed", "merged": true}`,
		"closed": `{"number": 12, "state": "closed", "merged": false}`,
	} {
		t.Run(name, func(t *testing.T) {
			var requests []recordedRequest
			writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(probeBody))
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"number": 63}`))
			})

			out, err := runCreatePull(createPullArgs{
				Owner: "o", Repo: "r", Title: "issue #57: daemon", Head: "task/57-daemon", Base: "main",
			})
			if err != nil {
				t.Fatalf("runCreatePull() = %v", err)
			}
			if strings.Contains(out, "already exists") {
				t.Errorf("runCreatePull() = %q, want a new pull request rather than the stale one", out)
			}
			if !strings.Contains(out, "#63") {
				t.Errorf("runCreatePull() = %q, want the newly opened pull request", out)
			}
			if len(requests) != 2 || requests[1].Method != http.MethodPost {
				t.Errorf("requests = %+v, want a probe then a create", requests)
			}
		})
	}
}

// TestCreatePullRejectsASlashedBase: the probe route is GET /pulls/{base}/{head} and only
// {head} is a catch-all (routers/api/v1/api.go:1642). A base of "release/1.0" would be read
// as base="release", so the probe would answer 404 for a branch that is not the one asked
// about - exactly the silent misread the probe exists to prevent.
func TestCreatePullRejectsASlashedBase(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("create-pull made a request despite an unprobeable base")
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "t", Head: "task/57-daemon", Base: "release/1.0",
	})
	if err == nil {
		t.Fatal("runCreatePull() accepted a base branch it cannot probe")
	}
	if !strings.Contains(err.Error(), "release/1.0") {
		t.Errorf("runCreatePull() = %v, want the base branch named", err)
	}
	var ue usageError
	if !errors.As(err, &ue) {
		t.Errorf("runCreatePull() = %T, want a usageError so the flag list is printed", err)
	}
}

// TestServerFailuresAreNotUsageErrors: a 500 is not answered with a wall of flag usage.
// emitWriteResult prints fs.Usage() only for a usageError, so this is what keeps the one
// line that says what actually broke from being buried.
func TestServerFailuresAreNotUsageErrors(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := runComment(commentArgs{Owner: "o", Repo: "r", Issue: 57, Body: "note"})
	if err == nil {
		t.Fatal("runComment() ignored a 500")
	}
	var ue usageError
	if errors.As(err, &ue) {
		t.Errorf("runComment() = %v, want a plain error rather than one that prints the flag list", err)
	}

	// …while a missing flag still is one.
	_, err = runComment(commentArgs{Owner: "o", Repo: "r", Issue: 57})
	if !errors.As(err, &ue) {
		t.Errorf("runComment() = %T for a missing --body, want a usageError", err)
	}
}

// TestCreatePullProbeFailureIsNotTreatedAsAbsence: a 500 on the probe must not be read as
// "no pull request yet", which would open a duplicate the moment the instance recovers.
func TestCreatePullProbeFailureIsNotTreatedAsAbsence(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("create-pull posted after a failed probe")
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "t", Head: "h", Base: "main",
	}); err == nil {
		t.Fatal("runCreatePull() ignored a failed existence probe")
	}
}

// TestCreatePullDraftUsesTheWIPPrefix: CreatePullRequestOption has no draft field
// (modules/structs/pull.go:119-144); Gitea decides draft-ness from the title prefix.
func TestCreatePullDraftUsesTheWIPPrefix(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 1}`))
	})

	if _, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "issue #57: daemon", Head: "h", Base: "main",
		Draft: true, WIPPrefix: defaultWIPPrefix,
	}); err != nil {
		t.Fatalf("runCreatePull() = %v", err)
	}
	if !strings.Contains(requests[1].Body, `"WIP: issue #57: daemon"`) {
		t.Errorf("create body = %q, want a WIP-prefixed title", requests[1].Body)
	}
}

// TestCreatePullHonoursACustomWIPPrefix is the only case --wip-prefix exists for.
//
// The default prefix needs no flag — runCreatePull falls back to defaultWIPPrefix — so a test
// that passes the default (or the zero value) exercises the fallback, not the flag. On an
// instance that changed PULL_REQUEST.WORK_IN_PROGRESS_PREFIXES, "WIP:" is not a
// work-in-progress marker at all: the pull request would open as an ordinary, immediately
// reviewable one while the bridge's config says draft, and the request would still succeed, so
// nothing anywhere would report it.
func TestCreatePullHonoursACustomWIPPrefix(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 1}`))
	})

	if _, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "issue #57: daemon", Head: "h", Base: "main",
		Draft: true, WIPPrefix: "[DRAFT]",
	}); err != nil {
		t.Fatalf("runCreatePull() = %v", err)
	}
	if !strings.Contains(requests[1].Body, `"[DRAFT] issue #57: daemon"`) {
		t.Errorf("create body = %q, want the configured prefix, not the shipped default", requests[1].Body)
	}
	if strings.Contains(requests[1].Body, defaultWIPPrefix) {
		t.Errorf("create body = %q, want no trace of the default prefix", requests[1].Body)
	}
}

func TestCreatePullDoesNotDoublePrefixADraft(t *testing.T) {
	var requests []recordedRequest
	writeTestServer(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 1}`))
	})

	if _, err := runCreatePull(createPullArgs{
		Owner: "o", Repo: "r", Title: "WIP: already", Head: "h", Base: "main", Draft: true,
	}); err != nil {
		t.Fatalf("runCreatePull() = %v", err)
	}
	if strings.Contains(requests[1].Body, "WIP: WIP:") {
		t.Errorf("create body = %q", requests[1].Body)
	}
}

func TestCreatePullRequiresHeadAndBase(t *testing.T) {
	for name, args := range map[string]createPullArgs{
		"no head":  {Owner: "o", Repo: "r", Title: "t", Base: "main"},
		"no base":  {Owner: "o", Repo: "r", Title: "t", Head: "h"},
		"no title": {Owner: "o", Repo: "r", Head: "h", Base: "main"},
		"no repo":  {Owner: "o", Title: "t", Head: "h", Base: "main"},
	} {
		if _, err := runCreatePull(args); err == nil {
			t.Errorf("runCreatePull(%s) accepted incomplete arguments", name)
		}
	}
}

// TestEscapeBranchPathKeepsSlashes: the head is a catch-all path segment
// (routers/api/v1/api.go:1642), so an escaped %2F would not match it.
func TestEscapeBranchPathKeepsSlashes(t *testing.T) {
	if got := escapeBranchPath("task/57-daemon"); got != "task/57-daemon" {
		t.Errorf("escapeBranchPath() = %q", got)
	}
	if got := escapeBranchPath("feat/a b"); got != "feat/a%20b" {
		t.Errorf("escapeBranchPath() = %q", got)
	}
}

// TestRepoPathEscapesBothSegments: unlike a branch head, owner and repo are single path
// segments, so a slash in either is data rather than structure. The bridge feeds these from
// a YAML config and from `gitea-ref:` trailers parsed out of kanban task bodies, so they are
// no longer only what a human typed on the command line.
func TestRepoPathEscapesBothSegments(t *testing.T) {
	if got := repoPath("o", "r"); got != "o/r" {
		t.Errorf("repoPath() = %q", got)
	}
	if got := repoPath("..", "../admin"); got != "..%2F..%2Fadmin" && got != "../..%2Fadmin" {
		// url.PathEscape leaves "." alone; what matters is that no unescaped slash from the
		// repo segment reaches the path.
		t.Errorf("repoPath() = %q", got)
	}
	if strings.Count(repoPath("a/b", "c/d"), "/") != 1 {
		t.Errorf("repoPath() must contribute exactly one path separator: %q", repoPath("a/b", "c/d"))
	}
	if got := repoPath("my org", "my repo"); got != "my%20org/my%20repo" {
		t.Errorf("repoPath() = %q", got)
	}
}

func TestSplitLabelsTrimsAndDropsEmpties(t *testing.T) {
	got := splitLabels(" a , ,b ,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitLabels() = %#v", got)
	}
}

// TestEveryRequestIsBounded pins the deadline on the CLI's HTTP client.
//
// http.DefaultClient has none, so a half-open connection to the instance does not fail - it
// hangs, and the process never exits. The gitea-automations bridge shells out to these verbs
// from inside its outbound leg, so an unbounded write there stops it reading kanban events
// while it still looks alive: the "a dead watcher is silent" guard cannot fire, because the
// watcher is not dead.
func TestEveryRequestIsBounded(t *testing.T) {
	if httpClient.Timeout <= 0 {
		t.Fatal("httpClient must carry a timeout; http.DefaultClient's zero value never returns")
	}
	// And that no request goes around it. One file left on http.DefaultClient is one
	// unbounded call, which is all it takes; comment lines are skipped so the explanation
	// above the client may name it.
	for _, path := range []string{"main.go", "write.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "http.DefaultClient") {
				t.Errorf("%s:%d uses http.DefaultClient, which has no timeout; use httpClient", path, i+1)
			}
		}
	}
}
