package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/httpkit"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	defaultUserAgent = "ao-agent-orchestrator/tracker-gitea"

	// Gitea's default MAX_RESPONSE_ITEMS is 50; asking for more is clamped.
	listPageSize = 50
	// Cap on pages per List (50 pages * 50 = 2.5k issues) so a server that
	// never returns an empty page cannot loop forever.
	maxListPages = 50
)

// Sentinel errors, matched via errors.Is.
var (
	ErrNotFound       = errors.New("gitea tracker: issue not found")
	ErrRateLimited    = errors.New("gitea tracker: rate limited")
	ErrAuthFailed     = errors.New("gitea tracker: authentication failed")
	ErrWrongProvider  = errors.New("gitea tracker: id is not a gitea tracker id")
	ErrBadID          = errors.New("gitea tracker: malformed native id")
	ErrHostNotAllowed = errors.New("gitea tracker: host does not match configured instance")
	ErrBadBaseURL     = errors.New("gitea tracker: invalid base URL")
)

// Options configures a Tracker. Token and BaseURL are required.
//
// BaseURL is the Gitea instance root as a user would open it in a browser,
// e.g. "http://gitea.lan:3000" or "https://git.example.com/gitea". The
// adapter appends "/api/v1".
type Options struct {
	Token      TokenSource
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
}

// Tracker implements ports.Tracker against the Gitea REST API v1.
type Tracker struct {
	http      *http.Client
	userAgent string
	apiBase   string // <BaseURL>/api/v1
	host      string // lowercased host[:port] of BaseURL
	tokens    TokenSource
	preflight httpkit.PreflightCache
}

var _ ports.Tracker = (*Tracker)(nil)

// New returns a Tracker. It fails fast on a missing token or malformed base
// URL so the daemon logs the problem at startup rather than at first lookup.
func New(opts Options) (*Tracker, error) {
	if opts.Token == nil {
		return nil, ErrNoToken
	}
	if _, err := opts.Token.Token(context.Background()); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: %q (want http(s)://host[:port][/path])", ErrBadBaseURL, opts.BaseURL)
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	t := &Tracker{
		http:      opts.HTTPClient,
		userAgent: ua,
		apiBase:   u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/") + "/api/v1",
		host:      strings.ToLower(u.Host),
		tokens:    opts.Token,
	}
	if t.http == nil {
		t.http = &http.Client{Timeout: 30 * time.Second}
	}
	return t, nil
}

// checkHost accepts "" (the configured instance) or the configured host
// itself; anything else is rejected so the token never leaves its instance.
func (t *Tracker) checkHost(host string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == t.host {
		return nil
	}
	return fmt.Errorf("%w: %q (configured %q)", ErrHostNotAllowed, host, t.host)
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

type gtIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	Labels    []gtLabel `json:"labels"`
	Assignees []gtUser  `json:"assignees"`
	// PullRequest is non-null when the item is a PR; the issues endpoint
	// returns both kinds.
	PullRequest *struct{} `json:"pull_request"`
}

type gtLabel struct {
	Name string `json:"name"`
}

type gtUser struct {
	Login string `json:"login"`
}

// Get fetches a single issue by id and maps it onto the normalized domain.Issue.
func (t *Tracker) Get(ctx context.Context, id domain.TrackerID) (domain.Issue, error) {
	if id.Provider != domain.TrackerProviderGitea {
		return domain.Issue{}, fmt.Errorf("%w: provider=%q", ErrWrongProvider, id.Provider)
	}
	repo, n, err := parseGiteaID(id.Native)
	if err != nil {
		return domain.Issue{}, err
	}
	if err := t.checkHost(id.Host); err != nil {
		return domain.Issue{}, err
	}
	body, _, err := t.roundTrip(ctx, fmt.Sprintf("/repos/%s/issues/%d", escapeRepo(repo), n))
	if err != nil {
		return domain.Issue{}, err
	}
	var raw gtIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.Issue{}, fmt.Errorf("gitea tracker: decode issue: %w", err)
	}
	return issueFromGitea(repo, id.Host, raw), nil
}

func issueFromGitea(repo, host string, raw gtIssue) domain.Issue {
	out := domain.Issue{
		ID: domain.TrackerID{
			Provider: domain.TrackerProviderGitea,
			Native:   fmt.Sprintf("%s#%d", repo, raw.Number),
			Host:     host,
		},
		Title: raw.Title,
		Body:  raw.Body,
		State: domain.IssueOpen,
		URL:   raw.HTMLURL,
	}
	if strings.EqualFold(raw.State, "closed") {
		out.State = domain.IssueDone
	}
	for _, l := range raw.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, a := range raw.Assignees {
		out.Assignees = append(out.Assignees, a.Login)
	}
	return out
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List returns a repo's issues (never PRs), filtered by state/labels/assignee.
// Pages are requested by number, so a Link header built from a ROOT_URL that
// differs from AO_GITEA_URL (host or sub-path) cannot misroute pagination.
// Paging stops at X-Total-Count, an empty page, or filter.Limit.
func (t *Tracker) List(ctx context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	if repo.Provider != domain.TrackerProviderGitea {
		return nil, fmt.Errorf("%w: provider=%q", ErrWrongProvider, repo.Provider)
	}
	if err := validateRepo(repo.Native); err != nil {
		return nil, err
	}
	if err := t.checkHost(repo.Host); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("type", "issues")
	switch filter.State {
	case domain.ListOpen:
		q.Set("state", "open")
	case domain.ListClosed:
		q.Set("state", "closed")
	default:
		q.Set("state", "all")
	}
	if len(filter.Labels) > 0 {
		q.Set("labels", strings.Join(filter.Labels, ","))
	}
	if filter.Assignee != "" && filter.Assignee != "*" {
		q.Set("assigned_by", filter.Assignee)
	}
	q.Set("limit", strconv.Itoa(listPageSize))

	out := make([]domain.Issue, 0)
	seen := 0
	for page := 1; ; page++ {
		if page > maxListPages {
			return nil, fmt.Errorf("gitea tracker: list pagination exceeded %d pages", maxListPages)
		}
		q.Set("page", strconv.Itoa(page))
		body, hdr, err := t.roundTrip(ctx, fmt.Sprintf("/repos/%s/issues?%s", escapeRepo(repo.Native), q.Encode()))
		if err != nil {
			return nil, err
		}
		var raw []gtIssue
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("gitea tracker: decode list: %w", err)
		}
		if len(raw) == 0 {
			break
		}
		seen += len(raw)
		pageIssues := make([]domain.Issue, 0, len(raw))
		for _, r := range raw {
			// Older Gitea ignores type=issues and assigned_by; enforce both here.
			if r.PullRequest != nil {
				continue
			}
			issue := issueFromGitea(repo.Native, repo.Host, r)
			if !assignedTo(issue.Assignees, filter.Assignee) {
				continue
			}
			pageIssues = append(pageIssues, issue)
		}
		var done bool
		if out, done = httpkit.AppendIssuesWithLimit(out, pageIssues, filter.Limit); done {
			break
		}
		if total, err := strconv.Atoi(hdr.Get("X-Total-Count")); err == nil && seen >= total {
			break
		}
	}
	return out, nil
}

// assignedTo reports whether an issue passes the assignee filter: "" matches
// everything, "*" any assigned issue, otherwise a case-insensitive login match.
func assignedTo(assignees []string, want string) bool {
	switch want {
	case "":
		return true
	case "*":
		return len(assignees) > 0
	}
	for _, a := range assignees {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Preflight
// ---------------------------------------------------------------------------

// Preflight verifies the token is accepted by the configured instance with
// one GET /user. Success is cached; failures are not.
func (t *Tracker) Preflight(ctx context.Context) error {
	return t.preflight.Run(ctx, func(ctx context.Context) error {
		_, _, err := t.roundTrip(ctx, "/user")
		return err
	})
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (t *Tracker) roundTrip(ctx context.Context, path string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.apiBase+path, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("gitea tracker: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", t.userAgent)
	tok, err := t.tokens.Token(ctx)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "token "+tok)

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("gitea tracker: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("gitea tracker: read response body: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, resp.Header, nil
	}
	return nil, nil, classifyError(resp, body)
}

func classifyError(resp *http.Response, body []byte) error {
	msg := httpkit.Message(body)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case http.StatusTooManyRequests:
		return httpkit.BuildRateLimitError(resp, msg, ErrRateLimited)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuthFailed, msg)
	}
	return fmt.Errorf("gitea tracker: %d %s", resp.StatusCode, msg)
}

// ---------------------------------------------------------------------------
// ID parsing
// ---------------------------------------------------------------------------

// parseGiteaID accepts "owner/repo#N".
func parseGiteaID(native string) (string, int, error) {
	repo, num, ok := strings.Cut(native, "#")
	if !ok {
		return "", 0, fmt.Errorf("%w: missing #index", ErrBadID)
	}
	if err := validateRepo(repo); err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%w: bad index %q", ErrBadID, num)
	}
	return repo, n, nil
}

// validateRepo requires exactly "owner/repo" (Gitea has no nested groups).
func validateRepo(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " \t\n\r#") {
		return fmt.Errorf("%w: repo must be owner/repo, got %q", ErrBadID, repo)
	}
	return nil
}

func escapeRepo(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}
