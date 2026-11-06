package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fakeGitea is a programmable httptest.Server keyed by request path.
// Unmatched requests fail the test.
type fakeGitea struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	handlers map[string]http.HandlerFunc
}

func newFakeGitea(t *testing.T) *fakeGitea {
	t.Helper()
	f := &fakeGitea{t: t, handlers: map[string]http.HandlerFunc{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		h, ok := f.handlers[r.URL.Path]
		f.mu.Unlock()
		if !ok || r.Method != http.MethodGet {
			f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "no handler", http.StatusNotImplemented)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitea) on(path string, h http.HandlerFunc) { f.handlers[path] = h }

func (f *fakeGitea) calls() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

func (f *fakeGitea) host() string { return strings.TrimPrefix(f.server.URL, "http://") }

func json200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// jsonList serves body on every page with Gitea's X-Total-Count header.
func jsonList(total int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		_, _ = w.Write([]byte(body))
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func newTrackerForTest(t *testing.T, f *fakeGitea) *Tracker {
	t.Helper()
	tr, err := New(Options{Token: StaticTokenSource("tok"), BaseURL: f.server.URL, HTTPClient: f.server.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

func giteaID(native string) domain.TrackerID {
	return domain.TrackerID{Provider: domain.TrackerProviderGitea, Native: native}
}

const issue7 = `{"number":7,"title":"Fix it","body":"details","state":"open",
 "html_url":"http://gitea.lan/owner/repo/issues/7",
 "labels":[{"name":"bug"},{"name":"p1"}],"assignees":[{"login":"alice"}]}`

func TestNew_Validation(t *testing.T) {
	if _, err := New(Options{BaseURL: "http://gitea.lan"}); !errors.Is(err, ErrNoToken) {
		t.Errorf("nil token: err = %v, want ErrNoToken", err)
	}
	if _, err := New(Options{Token: StaticTokenSource(" "), BaseURL: "http://gitea.lan"}); !errors.Is(err, ErrNoToken) {
		t.Errorf("blank token: err = %v, want ErrNoToken", err)
	}
	for _, bad := range []string{"", "gitea.lan:3000", "ftp://gitea.lan", "http://"} {
		if _, err := New(Options{Token: StaticTokenSource("tok"), BaseURL: bad}); !errors.Is(err, ErrBadBaseURL) {
			t.Errorf("BaseURL %q: err = %v, want ErrBadBaseURL", bad, err)
		}
	}
	tr, err := New(Options{Token: StaticTokenSource("tok"), BaseURL: "https://Git.Example.com:8443/gitea/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr.apiBase != "https://Git.Example.com:8443/gitea/api/v1" || tr.host != "git.example.com:8443" {
		t.Errorf("apiBase=%q host=%q", tr.apiBase, tr.host)
	}
}

func TestEnvTokenSource_Precedence(t *testing.T) {
	t.Setenv("AO_GITEA_TOKEN", "")
	t.Setenv("GITEA_TOKEN", "")
	if _, err := (EnvTokenSource{}).Token(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Errorf("no env: err = %v, want ErrNoToken", err)
	}
	t.Setenv("GITEA_TOKEN", "global")
	if tok, _ := (EnvTokenSource{}).Token(context.Background()); tok != "global" {
		t.Errorf("tok = %q, want global", tok)
	}
	t.Setenv("AO_GITEA_TOKEN", "ao")
	if tok, _ := (EnvTokenSource{}).Token(context.Background()); tok != "ao" {
		t.Errorf("tok = %q, want ao", tok)
	}
}

func TestParseGiteaID(t *testing.T) {
	if repo, n, err := parseGiteaID("owner/repo#12"); err != nil || repo != "owner/repo" || n != 12 {
		t.Errorf("got %q %d %v", repo, n, err)
	}
	for _, bad := range []string{"owner/repo", "repo#1", "a/b/c#1", "owner/repo#0", "owner/repo#x", "/repo#1", "own er/repo#1"} {
		if _, _, err := parseGiteaID(bad); !errors.Is(err, ErrBadID) {
			t.Errorf("%q: err = %v, want ErrBadID", bad, err)
		}
	}
}

func TestGet_HappyPath(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/api/v1/repos/owner/repo/issues/7", json200(issue7))
	got, err := newTrackerForTest(t, f).Get(context.Background(), giteaID("owner/repo#7"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := domain.Issue{
		ID:        giteaID("owner/repo#7"),
		Title:     "Fix it",
		Body:      "details",
		State:     domain.IssueOpen,
		URL:       "http://gitea.lan/owner/repo/issues/7",
		Labels:    []string{"bug", "p1"},
		Assignees: []string{"alice"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get =\n%+v\nwant\n%+v", got, want)
	}
	req := f.calls()[0]
	if h := req.Header.Get("Authorization"); h != "token tok" {
		t.Errorf("Authorization = %q, want %q", h, "token tok")
	}
}

func TestGet_ClosedMapsToDone_EmptyListsNil(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/api/v1/repos/owner/repo/issues/3", json200(`{"number":3,"state":"closed","labels":[],"assignees":null}`))
	got, err := newTrackerForTest(t, f).Get(context.Background(), giteaID("owner/repo#3"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != domain.IssueDone || got.Labels != nil || got.Assignees != nil {
		t.Errorf("got %+v", got)
	}
}

func TestGet_ErrorClassification(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusUnauthorized, ErrAuthFailed},
		{http.StatusForbidden, ErrAuthFailed},
		{http.StatusTooManyRequests, ErrRateLimited},
	}
	for _, c := range cases {
		f := newFakeGitea(t)
		f.on("/api/v1/repos/owner/repo/issues/1", status(c.code, `{"message":"nope"}`))
		_, err := newTrackerForTest(t, f).Get(context.Background(), giteaID("owner/repo#1"))
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), "nope") {
			t.Errorf("%d: err = %v, want %v with message", c.code, err, c.want)
		}
	}
}

func TestGet_RejectsWrongProvider(t *testing.T) {
	f := newFakeGitea(t)
	_, err := newTrackerForTest(t, f).Get(context.Background(), domain.TrackerID{Provider: domain.TrackerProviderGitLab, Native: "owner/repo#1"})
	if !errors.Is(err, ErrWrongProvider) {
		t.Errorf("err = %v, want ErrWrongProvider", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("made %d requests, want 0", n)
	}
}

func TestGet_HostRouting(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/api/v1/repos/owner/repo/issues/7", json200(issue7))
	tr := newTrackerForTest(t, f)

	// The configured host (case-insensitive) is accepted and preserved.
	id := giteaID("owner/repo#7")
	id.Host = strings.ToUpper(f.host())
	got, err := tr.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("configured host: %v", err)
	}
	if got.ID.Host != id.Host {
		t.Errorf("Host = %q, want %q", got.ID.Host, id.Host)
	}

	// Any other host is rejected before a request (and token) goes out.
	id.Host = "evil.example.com"
	if _, err := tr.Get(context.Background(), id); !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("other host: err = %v, want ErrHostNotAllowed", err)
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("made %d requests, want 1", n)
	}
}

func TestList_QueryEncoding(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/api/v1/repos/owner/repo/issues", json200(`[]`))
	tr := newTrackerForTest(t, f)
	repo := domain.TrackerRepo{Provider: domain.TrackerProviderGitea, Native: "owner/repo"}

	cases := []struct {
		filter domain.ListFilter
		want   string
	}{
		{domain.ListFilter{}, "limit=50&page=1&state=all&type=issues"},
		{domain.ListFilter{State: domain.ListOpen, Labels: []string{"bug", "p1"}, Assignee: "alice"},
			"assigned_by=alice&labels=bug%2Cp1&limit=50&page=1&state=open&type=issues"},
		{domain.ListFilter{State: domain.ListClosed}, "limit=50&page=1&state=closed&type=issues"},
		{domain.ListFilter{Assignee: "*"}, "limit=50&page=1&state=all&type=issues"},
	}
	for i, c := range cases {
		if _, err := tr.List(context.Background(), repo, c.filter); err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := f.calls()[i].URL.RawQuery; got != c.want {
			t.Errorf("query = %q, want %q", got, c.want)
		}
	}
}

// pagedIssues serves pages of n numbered issues by the page query param,
// optionally with X-Total-Count, and a Link header that points somewhere
// bogus (as a ROOT_URL mismatch would) to prove it is ignored.
func pagedIssues(t *testing.T, pages [][]int, total bool) http.HandlerFunc {
	count := 0
	for _, p := range pages {
		count += len(p)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			t.Errorf("page = %q, want >= 1", r.URL.Query().Get("page"))
			return
		}
		w.Header().Set("Link", `<http://root-url.example/wrong/prefix/issues?page=99>; rel="next"`)
		if total {
			w.Header().Set("X-Total-Count", strconv.Itoa(count))
		}
		items := []string{}
		if page <= len(pages) {
			for _, n := range pages[page-1] {
				items = append(items, fmt.Sprintf(`{"number":%d}`, n))
			}
		}
		_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	}
}

func listNatives(t *testing.T, tr *Tracker, filter domain.ListFilter) []string {
	t.Helper()
	got, err := tr.List(context.Background(), domain.TrackerRepo{Provider: domain.TrackerProviderGitea, Native: "owner/repo"}, filter)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	natives := []string{}
	for _, is := range got {
		natives = append(natives, is.ID.Native)
	}
	return natives
}

func TestList_PaginatesByPageNumber(t *testing.T) {
	cases := []struct {
		name      string
		total     bool
		limit     int
		want      string
		wantCalls int
	}{
		{"stops at X-Total-Count", true, 0, "[owner/repo#1 owner/repo#2 owner/repo#3]", 2},
		{"stops at empty page without total", false, 0, "[owner/repo#1 owner/repo#2 owner/repo#3]", 3},
		{"stops at limit", true, 2, "[owner/repo#1 owner/repo#2]", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGitea(t)
			f.on("/api/v1/repos/owner/repo/issues", pagedIssues(t, [][]int{{1, 2}, {3}}, c.total))
			got := listNatives(t, newTrackerForTest(t, f), domain.ListFilter{Limit: c.limit})
			if fmt.Sprint(got) != c.want {
				t.Errorf("natives = %v, want %s", got, c.want)
			}
			if n := len(f.calls()); n != c.wantCalls {
				t.Errorf("requests = %d, want %d", n, c.wantCalls)
			}
		})
	}
}

func TestList_SubpathBaseURL(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/gitea/api/v1/repos/owner/repo/issues", pagedIssues(t, [][]int{{1}, {2}}, true))
	tr, err := New(Options{Token: StaticTokenSource("tok"), BaseURL: f.server.URL + "/gitea/", HTTPClient: f.server.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := fmt.Sprint(listNatives(t, tr, domain.ListFilter{})); got != "[owner/repo#1 owner/repo#2]" {
		t.Errorf("natives = %s", got)
	}
}

func TestList_DropsPullRequests(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/api/v1/repos/owner/repo/issues", jsonList(3, `[
		{"number":1,"pull_request":null},
		{"number":2,"pull_request":{"merged":false}},
		{"number":3}]`))
	if got := fmt.Sprint(listNatives(t, newTrackerForTest(t, f), domain.ListFilter{})); got != "[owner/repo#1 owner/repo#3]" {
		t.Errorf("natives = %s, want PR #2 dropped", got)
	}
}

// Older Gitea ignores assigned_by and returns everything; the adapter must
// still honor the filter.
func TestList_EnforcesAssigneeLocally(t *testing.T) {
	f := newFakeGitea(t)
	f.on("/api/v1/repos/owner/repo/issues", jsonList(3, `[
		{"number":1,"assignees":[{"login":"Alice"}]},
		{"number":2,"assignees":[{"login":"bob"}]},
		{"number":3,"assignees":null}]`))
	tr := newTrackerForTest(t, f)
	for filter, want := range map[string]string{
		"alice": "[owner/repo#1]",
		"*":     "[owner/repo#1 owner/repo#2]",
		"":      "[owner/repo#1 owner/repo#2 owner/repo#3]",
	} {
		if got := fmt.Sprint(listNatives(t, tr, domain.ListFilter{Assignee: filter})); got != want {
			t.Errorf("assignee %q: natives = %s, want %s", filter, got, want)
		}
	}
}

func TestList_RejectsBadInput(t *testing.T) {
	f := newFakeGitea(t)
	tr := newTrackerForTest(t, f)
	if _, err := tr.List(context.Background(), domain.TrackerRepo{Provider: domain.TrackerProviderGitHub, Native: "owner/repo"}, domain.ListFilter{}); !errors.Is(err, ErrWrongProvider) {
		t.Errorf("wrong provider: err = %v", err)
	}
	if _, err := tr.List(context.Background(), domain.TrackerRepo{Provider: domain.TrackerProviderGitea, Native: "group/sub/repo"}, domain.ListFilter{}); !errors.Is(err, ErrBadID) {
		t.Errorf("nested repo: err = %v", err)
	}
	if _, err := tr.List(context.Background(), domain.TrackerRepo{Provider: domain.TrackerProviderGitea, Native: "owner/repo", Host: "other:3000"}, domain.ListFilter{}); !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("other host: err = %v", err)
	}
}

func TestPreflight_CachesSuccessNotFailure(t *testing.T) {
	f := newFakeGitea(t)
	fail := true
	f.on("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			status(http.StatusUnauthorized, `{"message":"bad token"}`)(w, nil)
			return
		}
		json200(`{"login":"me"}`)(w, nil)
	})
	tr := newTrackerForTest(t, f)
	if err := tr.Preflight(context.Background()); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("first: err = %v, want ErrAuthFailed", err)
	}
	fail = false
	for range 2 {
		if err := tr.Preflight(context.Background()); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("requests = %d, want 2 (failure not cached, success cached)", n)
	}
}
