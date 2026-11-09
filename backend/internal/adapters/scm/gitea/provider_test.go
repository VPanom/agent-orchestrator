package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// fakeGitea serves canned JSON per "METHOD path" and records requests. The
// instance lives under a /gitea sub-path to exercise AO_GITEA_URL prefixes.
type fakeGitea struct {
	t      *testing.T
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	seen   []string
	bodies map[string]string
}

func newFake(t *testing.T) (*fakeGitea, *Provider, *httptest.Server) {
	t.Helper()
	f := &fakeGitea{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}, bodies: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token secret" {
			t.Errorf("Authorization = %q", got)
		}
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/gitea/api/v1")
		f.seen = append(f.seen, key+"?"+r.URL.RawQuery)
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			f.bodies[key] = string(b)
		}
		h, ok := f.routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	p, err := NewProvider(ProviderOptions{BaseURL: srv.URL + "/gitea/", Token: StaticTokenSource("secret")})
	if err != nil {
		t.Fatal(err)
	}
	return f, p, srv
}

func (f *fakeGitea) json(key string, v any) {
	f.routes[key] = func(w http.ResponseWriter, r *http.Request) {
		if _, list := v.([]any); list && r.URL.Query().Get("page") > "1" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
}

func repoOf(p *Provider) ports.SCMRepo {
	r, ok := p.ParseRepository(p.BaseURL() + "/owner/repo.git")
	if !ok {
		panic("parse")
	}
	return r
}

func pr(number int, branch string, extra map[string]any) map[string]any {
	m := map[string]any{
		"id": 100 + number, "number": number, "title": "Feature", "state": "open", "mergeable": true,
		"html_url":   "https://gitea.example/owner/repo/pulls/" + strconv.Itoa(number),
		"user":       map[string]any{"login": "victor"},
		"head":       map[string]any{"ref": branch, "sha": testSHA, "repo": map[string]any{"full_name": "owner/repo"}},
		"base":       map[string]any{"ref": "main", "sha": "b", "repo": map[string]any{"full_name": "owner/repo"}},
		"merge_base": "base-sha",
		"updated_at": "2026-11-01T10:00:00Z",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestParseRepository(t *testing.T) {
	p, err := NewProvider(ProviderOptions{BaseURL: "https://gitea.panom.se", Token: StaticTokenSource("x")})
	if err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{
		"ssh://git@gitea.panom.se:30009/owner/repo.git",
		"git@gitea.panom.se:owner/repo.git",
		"https://gitea.panom.se/owner/repo",
	} {
		got, ok := p.ParseRepository(remote)
		want := ports.SCMRepo{Provider: "gitea", Host: "gitea.panom.se", Owner: "owner", Name: "repo", Repo: "owner/repo"}
		if !ok || got != want {
			t.Errorf("ParseRepository(%q) = %+v, %v", remote, got, ok)
		}
	}
	for _, remote := range []string{
		"https://github.com/owner/repo",
		"https://evil.example/owner/repo",
		"https://gitea.panom.se:8443/owner/repo",
		"https://gitea.panom.se/owner/repo/pulls/3",
	} {
		if _, ok := p.ParseRepository(remote); ok {
			t.Errorf("ParseRepository(%q) matched", remote)
		}
	}
}

func TestHostNotAllowedNeverSendsToken(t *testing.T) {
	f, p, _ := newFake(t)
	repo := repoOf(p)
	repo.Host = "evil.example"
	_, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: repo, Number: 1}})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	if len(f.seen) != 0 {
		t.Fatalf("requests sent: %v", f.seen)
	}
}

func TestFetchPullRequests(t *testing.T) {
	f, p, _ := newFake(t)
	f.json("GET /repos/owner/repo/pulls/7", pr(7, "feat", map[string]any{"requested_reviewers": []any{map[string]any{"login": "bob"}}}))
	f.json("GET /repos/owner/repo/commits/"+testSHA+"/status", map[string]any{
		"state": "failure", "total_count": 2,
		"statuses": []any{
			map[string]any{"id": 1, "status": "success", "context": "CI / lint (push)"},
			map[string]any{"id": 2, "status": "failure", "context": "CI / test (push)", "target_url": "https://gitea.example/owner/repo/actions/runs/4/jobs/1"},
		},
	})
	f.json("GET /repos/owner/repo/pulls/7/reviews", []any{})
	repo := repoOf(p)
	obs, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: repo, Number: 7}})
	if err != nil {
		t.Fatal(err)
	}
	o := obs[0]
	if !o.Fetched || o.Provider != "gitea" || o.Repo != "owner/repo" || o.Host != repo.Host {
		t.Fatalf("obs = %+v", o)
	}
	if o.PR.State != string(domain.PRStateOpen) || o.PR.SourceBranch != "feat" || o.PR.HeadSHA != testSHA || o.PR.BaseSHA != "base-sha" {
		t.Fatalf("pr = %+v", o.PR)
	}
	if o.CI.Summary != string(domain.CIFailing) || len(o.CI.Checks) != 2 || len(o.CI.FailedChecks) != 1 || o.CI.FailedChecks[0].Name != "CI / test (push)" || o.CI.FailedFingerprint == "" {
		t.Fatalf("ci = %+v", o.CI)
	}
	if o.Review.Decision != string(domain.ReviewRequired) {
		t.Fatalf("review = %+v", o.Review)
	}
	if o.Mergeability.State != string(domain.MergeBlocked) || strings.Join(o.Mergeability.Blockers, ",") != "ci_failing,review_required" {
		t.Fatalf("mergeability = %+v", o.Mergeability)
	}
}

func TestFetchPullRequestsNotFoundPlaceholder(t *testing.T) {
	_, p, _ := newFake(t)
	obs, err := p.FetchPullRequests(context.Background(), []ports.SCMPRRef{{Repo: repoOf(p), Number: 9}})
	if !errors.Is(err, ports.ErrSCMNotFound) || obs[0].Fetched || !errors.Is(obs[0].Error, ports.ErrSCMNotFound) {
		t.Fatalf("err=%v obs=%+v", err, obs)
	}
}

func TestStateAndMergeabilityMapping(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		state domain.PRState
		merge domain.Mergeability
	}{
		{"wip draft", map[string]any{"title": "WIP: thing"}, domain.PRStateDraft, domain.MergeBlocked},
		{"draft flag", map[string]any{"draft": true}, domain.PRStateDraft, domain.MergeBlocked},
		{"conflict", map[string]any{"mergeable": false}, domain.PRStateOpen, domain.MergeConflicting},
		{"merged", map[string]any{"state": "closed", "merged": true}, domain.PRStateMerged, domain.MergeUnknown},
		{"closed", map[string]any{"state": "closed"}, domain.PRStateClosed, domain.MergeUnknown},
		{"clean", nil, domain.PRStateOpen, domain.MergeMergeable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(pr(1, "b", tc.extra))
			var g giteaPR
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatal(err)
			}
			if got := toPRObservation(ports.SCMRepo{Repo: "owner/repo"}, &g).State; got != string(tc.state) {
				t.Errorf("state = %s, want %s", got, tc.state)
			}
			if got := mergeability(&g, string(domain.CIPassing), domain.ReviewNone).State; got != string(tc.merge) {
				t.Errorf("mergeability = %s, want %s", got, tc.merge)
			}
		})
	}
}

func TestReviewDecision(t *testing.T) {
	u := func(l string) *giteaUser { return &giteaUser{Login: l} }
	cases := []struct {
		name    string
		reviews []giteaReview
		req     bool
		want    domain.ReviewDecision
	}{
		{"none", nil, false, domain.ReviewNone},
		{"required", nil, true, domain.ReviewRequired},
		{"approved", []giteaReview{{User: u("a"), State: "APPROVED"}}, true, domain.ReviewApproved},
		{"changes win", []giteaReview{{User: u("a"), State: "APPROVED"}, {User: u("b"), State: "REQUEST_CHANGES"}}, false, domain.ReviewChangesRequest},
		{"latest per user", []giteaReview{{User: u("a"), State: "REQUEST_CHANGES"}, {User: u("a"), State: "APPROVED"}}, false, domain.ReviewApproved},
		{"dismissed ignored", []giteaReview{{User: u("a"), State: "REQUEST_CHANGES", Dismissed: true}}, false, domain.ReviewNone},
		{"comment ignored", []giteaReview{{User: u("a"), State: "COMMENT"}}, false, domain.ReviewNone},
	}
	for _, tc := range cases {
		if got := reviewDecision(tc.reviews, tc.req); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestListPRsByRepoStopsAtCursor(t *testing.T) {
	f, p, _ := newFake(t)
	f.json("GET /repos/owner/repo/pulls", []any{
		pr(3, "a", map[string]any{"updated_at": "2026-11-03T00:00:00Z"}),
		pr(2, "b", map[string]any{"updated_at": "2026-10-01T00:00:00Z"}),
	})
	got, err := p.ListPRsByRepo(context.Background(), repoOf(p), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 3 {
		t.Fatalf("got %+v", got)
	}
	q, _ := url.ParseQuery(strings.SplitN(f.seen[0], "?", 2)[1])
	if q.Get("state") != "all" || q.Get("sort") != "recentupdate" || q.Get("page") != "1" {
		t.Fatalf("query = %v", q)
	}
}

func TestListPRsByRepoPaginatesUntilEmptyPage(t *testing.T) {
	f, p, _ := newFake(t)
	f.routes["GET /repos/owner/repo/pulls"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_ = json.NewEncoder(w).Encode([]any{pr(1, "a", nil)})
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}
	got, err := p.ListPRsByRepo(context.Background(), repoOf(p), time.Time{})
	if err != nil || len(got) != 1 || len(f.seen) != 2 {
		t.Fatalf("got=%v err=%v seen=%v", got, err, f.seen)
	}
}

func TestListOpenPRsByBranchSkipsForksAndOtherBranches(t *testing.T) {
	f, p, _ := newFake(t)
	fork := pr(2, "feat", nil)
	fork["head"] = map[string]any{"ref": "feat", "sha": testSHA, "repo": map[string]any{"full_name": "someone/repo"}}
	f.json("GET /repos/owner/repo/pulls", []any{pr(1, "feat", nil), fork, pr(3, "other", nil)})
	got, err := p.ListOpenPRsByBranch(context.Background(), repoOf(p), "feat")
	if err != nil || len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestFetchReviewThreads(t *testing.T) {
	f, p, _ := newFake(t)
	f.json("GET /repos/owner/repo/pulls/5", pr(5, "feat", nil))
	f.json("GET /repos/owner/repo/pulls/5/reviews", []any{
		map[string]any{"id": 11, "user": map[string]any{"login": "bob"}, "state": "REQUEST_CHANGES", "body": "fix it", "commit_id": testSHA, "comments_count": 2},
		map[string]any{"id": 12, "user": map[string]any{"login": "victor"}, "state": "COMMENT", "body": "", "comments_count": 1},
		map[string]any{"id": 13, "user": map[string]any{"login": "bob"}, "state": "PENDING", "comments_count": 1},
	})
	f.json("GET /repos/owner/repo/pulls/5/reviews/11/comments", []any{
		map[string]any{"id": 21, "body": "bug", "user": map[string]any{"login": "bob"}, "path": "a.go", "position": 10, "pull_request_review_id": 11},
		map[string]any{"id": 22, "body": "nit", "user": map[string]any{"login": "bob"}, "path": "b.go", "position": 3, "pull_request_review_id": 11, "resolver": map[string]any{"login": "victor"}},
	})
	f.json("GET /repos/owner/repo/pulls/5/reviews/12/comments", []any{
		map[string]any{"id": 23, "body": "done", "user": map[string]any{"login": "victor"}, "path": "a.go", "position": 10, "pull_request_review_id": 12},
	})
	got, err := p.FetchReviewThreads(context.Background(), ports.SCMPRRef{Repo: repoOf(p), Number: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != string(domain.ReviewChangesRequest) || len(got.Reviews) != 1 || got.Reviews[0].Body != "fix it" {
		t.Fatalf("decision/reviews = %+v", got)
	}
	if len(got.Threads) != 2 {
		t.Fatalf("threads = %+v", got.Threads)
	}
	a, b := got.Threads[0], got.Threads[1]
	if a.ID != "21" || a.Path != "a.go" || a.Line != 10 || a.Resolved || len(a.Comments) != 2 || a.Comments[1].ReviewID != "12" {
		t.Fatalf("thread a = %+v", a)
	}
	if b.ID != "22" || !b.Resolved {
		t.Fatalf("thread b = %+v", b)
	}
	for _, s := range f.seen {
		if strings.Contains(s, "/reviews/13/") {
			t.Fatalf("pending review comments fetched: %v", f.seen)
		}
	}
}

func TestMergePullRequest(t *testing.T) {
	f, p, _ := newFake(t)
	f.routes["POST /repos/owner/repo/pulls/4/merge"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	f.json("GET /repos/owner/repo/pulls/4", pr(4, "x", map[string]any{"state": "closed", "merged": true, "merge_commit_sha": "m3rg3"}))
	ref := ports.SCMPRRef{Repo: repoOf(p), Number: 4}
	res, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: ref, ExpectedHeadSHA: testSHA, Method: ports.SCMMergeSquash})
	if err != nil || res.MergeCommitSHA != "m3rg3" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(f.bodies["POST /repos/owner/repo/pulls/4/merge"]), &body)
	if body["Do"] != "squash" || body["head_commit_id"] != testSHA {
		t.Fatalf("merge body = %v", body)
	}

	f.routes["POST /repos/owner/repo/pulls/4/merge"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) }
	if _, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: ref, ExpectedHeadSHA: testSHA, Method: ports.SCMMergeSquash}); !errors.Is(err, ports.ErrSCMHeadChanged) {
		t.Fatalf("409 err = %v", err)
	}
	f.routes["POST /repos/owner/repo/pulls/4/merge"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) }
	if _, err := p.MergePullRequest(context.Background(), ports.SCMMergeRequest{PR: ref, ExpectedHeadSHA: testSHA, Method: ports.SCMMergeSquash}); !errors.Is(err, ports.ErrSCMNotMergeable) {
		t.Fatalf("405 err = %v", err)
	}
}

func TestPostReviewAndRequestReview(t *testing.T) {
	f, p, _ := newFake(t)
	f.json("POST /repos/owner/repo/pulls/6/reviews", map[string]any{"id": 77})
	f.json("POST /repos/owner/repo/pulls/6/requested_reviewers", []any{})
	ref := ports.SCMPRRef{Repo: repoOf(p), Number: 6}
	id, err := p.PostReview(context.Background(), ports.SCMReviewPostRequest{PR: ref, CommitSHA: testSHA, Body: "summary", Comments: []ports.SCMReviewComment{{Path: "a.go", Line: 3, Body: "fix"}}})
	if err != nil || id != "77" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	var body struct {
		Event    string `json:"event"`
		CommitID string `json:"commit_id"`
		Comments []struct {
			Path        string `json:"path"`
			NewPosition int    `json:"new_position"`
		} `json:"comments"`
	}
	_ = json.Unmarshal([]byte(f.bodies["POST /repos/owner/repo/pulls/6/reviews"]), &body)
	if body.Event != "COMMENT" || body.CommitID != testSHA || len(body.Comments) != 1 || body.Comments[0].NewPosition != 3 {
		t.Fatalf("review body = %+v", body)
	}
	if _, err := p.PostReview(context.Background(), ports.SCMReviewPostRequest{PR: ref, Comments: []ports.SCMReviewComment{{Path: "a.go"}}}); err == nil {
		t.Fatal("expected invalid comment error")
	}
	if err := p.RequestReview(context.Background(), ports.SCMReviewRequest{PR: ref, Reviewer: "@bob"}); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies["POST /repos/owner/repo/pulls/6/requested_reviewers"]; !strings.Contains(got, `"bob"`) {
		t.Fatalf("requested_reviewers body = %s", got)
	}
}

func TestRateLimitAndAuthErrors(t *testing.T) {
	f, p, _ := newFake(t)
	f.routes["GET /user"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	_, err := p.AuthenticatedIdentity(context.Background())
	var rl interface{ GetRetryAfter() time.Duration }
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.GetRetryAfter() != 30*time.Second {
		t.Fatalf("429 err = %v", err)
	}
	f.routes["GET /user"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	if _, err := p.AuthenticatedIdentity(context.Background()); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("401 err = %v", err)
	}
	f.json("GET /user", map[string]any{"login": "victor"})
	id, err := p.AuthenticatedIdentityForHost(context.Background(), repoOf(p).Host)
	if err != nil || id.Login != "victor" || !id.Human {
		t.Fatalf("identity = %+v err=%v", id, err)
	}
	if _, err := p.AuthenticatedIdentityForHost(context.Background(), "evil.example"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("foreign host err = %v", err)
	}
}

func TestSCMCredentialsAvailable(t *testing.T) {
	p, _ := NewProvider(ProviderOptions{BaseURL: "https://g.example", Token: StaticTokenSource("")})
	if ok, err := p.SCMCredentialsAvailable(context.Background()); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, err := NewProvider(ProviderOptions{BaseURL: "ftp://g.example", Token: StaticTokenSource("x")}); !errors.Is(err, ErrBadBaseURL) {
		t.Fatalf("bad base err = %v", err)
	}
}
