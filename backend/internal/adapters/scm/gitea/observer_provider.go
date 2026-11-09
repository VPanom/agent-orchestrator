package gitea

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	maxFetchBatch               = 25
	fetchConcurrency            = 5
	reviewCommentLimitPerThread = 5
)

type giteaUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

type giteaBranch struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type giteaPR struct {
	ID                 int64       `json:"id"`
	Number             int         `json:"number"`
	Title              string      `json:"title"`
	State              string      `json:"state"`
	Draft              bool        `json:"draft"`
	HTMLURL            string      `json:"html_url"`
	Merged             bool        `json:"merged"`
	Mergeable          bool        `json:"mergeable"`
	MergeBase          string      `json:"merge_base"`
	MergeCommitSHA     string      `json:"merge_commit_sha"`
	User               giteaUser   `json:"user"`
	Head               giteaBranch `json:"head"`
	Base               giteaBranch `json:"base"`
	Additions          int         `json:"additions"`
	Deletions          int         `json:"deletions"`
	ChangedFiles       int         `json:"changed_files"`
	RequestedReviewers []giteaUser `json:"requested_reviewers"`
	CreatedAt          *time.Time  `json:"created_at"`
	UpdatedAt          *time.Time  `json:"updated_at"`
	MergedAt           *time.Time  `json:"merged_at"`
	ClosedAt           *time.Time  `json:"closed_at"`
}

func (pr *giteaPR) isDraft() bool {
	// Older instances have no draft flag; Gitea's default WIP prefixes mark one.
	t := strings.ToUpper(strings.TrimSpace(pr.Title))
	return pr.Draft || strings.HasPrefix(t, "WIP:") || strings.HasPrefix(t, "[WIP]")
}

func (pr *giteaPR) open() bool { return !pr.Merged && !strings.EqualFold(pr.State, "closed") }

func fullName(b giteaBranch, fallback string) string {
	if b.Repo != nil && b.Repo.FullName != "" {
		return b.Repo.FullName
	}
	return fallback
}

func toPRObservation(repo ports.SCMRepo, pr *giteaPR) ports.SCMPRObservation {
	state := domain.PRStateOpen
	switch {
	case pr.Merged:
		state = domain.PRStateMerged
	case !pr.open():
		state = domain.PRStateClosed
	case pr.isDraft():
		state = domain.PRStateDraft
	}
	baseSHA := pr.MergeBase
	if baseSHA == "" {
		baseSHA = pr.Base.SHA
	}
	return ports.SCMPRObservation{
		ProviderID:        strconv.FormatInt(pr.ID, 10),
		URL:               pr.HTMLURL,
		HTMLURL:           pr.HTMLURL,
		Number:            pr.Number,
		State:             string(state),
		Draft:             pr.isDraft(),
		Merged:            pr.Merged,
		Closed:            !pr.Merged && !pr.open(),
		SourceBranch:      pr.Head.Ref,
		HeadRepo:          fullName(pr.Head, repo.Repo),
		BaseRepo:          fullName(pr.Base, repo.Repo),
		TargetBranch:      pr.Base.Ref,
		HeadSHA:           pr.Head.SHA,
		Title:             pr.Title,
		Additions:         pr.Additions,
		Deletions:         pr.Deletions,
		ChangedFiles:      pr.ChangedFiles,
		Author:            pr.User.Login,
		AuthorAvatarURL:   pr.User.AvatarURL,
		BaseSHA:           baseSHA,
		MergeCommitSHA:    pr.MergeCommitSHA,
		ProviderState:     pr.State,
		ProviderMergeable: strconv.FormatBool(pr.Mergeable),
		CreatedAtProvider: safeTime(pr.CreatedAt),
		UpdatedAtProvider: safeTime(pr.UpdatedAt),
		MergedAtProvider:  safeTime(pr.MergedAt),
		ClosedAtProvider:  safeTime(pr.ClosedAt),
	}
}

func safeTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func hashBody(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// RepoPRListGuard hashes the most recently updated PR so the observer can
// skip a full listing when nothing changed. Gitea's API sends no ETags.
func (p *Provider) RepoPRListGuard(ctx context.Context, repo ports.SCMRepo, etag string) (ports.SCMGuardResult, error) {
	base, err := p.checkRepo(repo)
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	resp, err := p.do(ctx, http.MethodGet, base+"/pulls", url.Values{"state": {"all"}, "sort": {"recentupdate"}, "limit": {"1"}}, nil)
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	h := hashBody(resp.body)
	return ports.SCMGuardResult{ETag: h, NotModified: etag != "" && etag == h}, nil
}

// ListPRsByRepo lists PRs in every state, newest update first, stopping at
// the first PR older than updatedAfter (zero = full listing).
func (p *Provider) ListPRsByRepo(ctx context.Context, repo ports.SCMRepo, updatedAfter time.Time) ([]ports.SCMPRObservation, error) {
	base, err := p.checkRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []ports.SCMPRObservation
	_, err = paginate(ctx, p, base+"/pulls", url.Values{"state": {"all"}, "sort": {"recentupdate"}}, func(prs []giteaPR) bool {
		for i := range prs {
			if !updatedAfter.IsZero() && prs[i].UpdatedAt != nil && prs[i].UpdatedAt.Before(updatedAfter) {
				return false
			}
			out = append(out, toPRObservation(repo, &prs[i]))
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListOpenPRsByBranch lists open PRs whose head branch is branch in repo
// itself; fork PRs are dropped, matching the GitHub and GitLab listings.
func (p *Provider) ListOpenPRsByBranch(ctx context.Context, repo ports.SCMRepo, branch string) ([]ports.SCMPRObservation, error) {
	base, err := p.checkRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []ports.SCMPRObservation
	_, err = paginate(ctx, p, base+"/pulls", url.Values{"state": {"open"}}, func(prs []giteaPR) bool {
		for i := range prs {
			obs := toPRObservation(repo, &prs[i])
			if obs.SourceBranch != branch || !strings.EqualFold(obs.HeadRepo, obs.BaseRepo) {
				continue
			}
			out = append(out, obs)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type giteaCombinedStatus struct {
	State      string              `json:"state"`
	TotalCount int                 `json:"total_count"`
	Statuses   []giteaCommitStatus `json:"statuses"`
}

type giteaCommitStatus struct {
	ID int64 `json:"id"`
	// Gitea serializes a status's state as "status"; accept "state" too.
	Status    string `json:"status"`
	State     string `json:"state"`
	Context   string `json:"context"`
	TargetURL string `json:"target_url"`
}

func (s giteaCommitStatus) state() string {
	if s.Status != "" {
		return strings.ToLower(s.Status)
	}
	return strings.ToLower(s.State)
}

func (p *Provider) combinedStatus(ctx context.Context, repo ports.SCMRepo, sha string) (giteaCombinedStatus, []byte, error) {
	base, err := p.checkRepo(repo)
	if err != nil {
		return giteaCombinedStatus{}, nil, err
	}
	resp, err := p.do(ctx, http.MethodGet, base+"/commits/"+url.PathEscape(sha)+"/status", url.Values{"limit": {strconv.Itoa(pageSize)}}, nil)
	if err != nil {
		return giteaCombinedStatus{}, nil, err
	}
	var cs giteaCombinedStatus
	if err := json.Unmarshal(resp.body, &cs); err != nil {
		return giteaCombinedStatus{}, nil, fmt.Errorf("gitea scm: decode combined status: %w", err)
	}
	return cs, resp.body, nil
}

// CommitChecksGuard hashes the head commit's combined status.
func (p *Provider) CommitChecksGuard(ctx context.Context, repo ports.SCMRepo, headSHA, etag string) (ports.SCMGuardResult, error) {
	if headSHA == "" {
		return ports.SCMGuardResult{}, fmt.Errorf("gitea scm: empty head SHA: %w", ErrNotFound)
	}
	_, body, err := p.combinedStatus(ctx, repo, headSHA)
	if err != nil {
		return ports.SCMGuardResult{}, err
	}
	h := hashBody(body)
	return ports.SCMGuardResult{ETag: h, NotModified: etag != "" && etag == h}, nil
}

func (p *Provider) fetchCI(ctx context.Context, repo ports.SCMRepo, headSHA string) (ports.SCMCIObservation, error) {
	if headSHA == "" {
		return ports.SCMCIObservation{Summary: string(domain.CIUnknown)}, nil
	}
	cs, _, err := p.combinedStatus(ctx, repo, headSHA)
	if err != nil {
		return ports.SCMCIObservation{}, err
	}
	obs := ports.SCMCIObservation{Summary: string(domain.CIUnknown), HeadSHA: headSHA, Partial: cs.TotalCount > len(cs.Statuses)}
	if len(cs.Statuses) == 0 {
		return obs, nil
	}
	obs.Summary = string(combinedToCI(cs.State))
	for _, s := range cs.Statuses {
		status := statusToCheck(s.state())
		check := ports.SCMCheckObservation{Name: s.Context, Status: string(status), Conclusion: s.state(), URL: s.TargetURL, ProviderID: strconv.FormatInt(s.ID, 10)}
		obs.Checks = append(obs.Checks, check)
		if status == domain.PRCheckFailed {
			obs.FailedChecks = append(obs.FailedChecks, check)
		}
	}
	obs.FailedFingerprint = failedFingerprint(headSHA, obs.FailedChecks)
	return obs, nil
}

func combinedToCI(state string) domain.CIState {
	switch strings.ToLower(state) {
	case "success", "warning":
		return domain.CIPassing
	case "failure", "error":
		return domain.CIFailing
	case "pending", "running":
		return domain.CIPending
	default:
		return domain.CIUnknown
	}
}

func statusToCheck(state string) domain.PRCheckStatus {
	switch state {
	case "success", "warning":
		return domain.PRCheckPassed
	case "failure", "error":
		return domain.PRCheckFailed
	case "pending", "running":
		return domain.PRCheckInProgress
	case "skipped":
		return domain.PRCheckSkipped
	default:
		return domain.PRCheckUnknown
	}
}

func failedFingerprint(headSHA string, checks []ports.SCMCheckObservation) string {
	if len(checks) == 0 {
		return ""
	}
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = headSHA + "\x00" + c.Name + "\x00" + c.Status + "\x00" + c.Conclusion + "\x00" + c.URL + "\x00" + c.ProviderID
	}
	sort.Strings(parts)
	return hashBody([]byte(strings.Join(parts, "\x1e")))
}

// FetchFailedCheckLogTail returns no log. Commit statuses carry no job id,
// and the Actions job-log API only exists on recent Gitea releases.
// ponytail: map an Actions target_url (/actions/runs/R/jobs/J) to
// /actions/jobs/{id}/logs once the minimum supported Gitea exposes job ids.
func (p *Provider) FetchFailedCheckLogTail(context.Context, ports.SCMRepo, ports.SCMCheckObservation) (string, error) {
	return "", nil
}

// FetchPullRequests fetches PR detail, CI and review decision for up to 25
// refs. A failed ref yields a Fetched=false placeholder carrying its error,
// and the first error is also returned, mirroring the GitLab provider.
func (p *Provider) FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	if len(refs) > maxFetchBatch {
		return nil, fmt.Errorf("gitea scm: batch size %d exceeds limit %d", len(refs), maxFetchBatch)
	}
	results := make([]ports.SCMObservation, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	sem := make(chan struct{}, fetchConcurrency)
	for i, ref := range refs {
		wg.Add(1)
		go func(idx int, r ports.SCMPRRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			obs, err := p.fetchOne(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				p.logger.Warn("gitea scm: fetch PR failed", "repo", r.Repo.Repo, "pr", r.Number, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				obs = ports.SCMObservation{Provider: ProviderKey, Host: r.Repo.Host, Repo: r.Repo.Repo, PR: ports.SCMPRObservation{Number: r.Number, URL: r.URL}, Error: err}
			}
			results[idx] = obs
		}(i, ref)
	}
	wg.Wait()
	return results, firstErr
}

func (p *Provider) getPR(ctx context.Context, repo ports.SCMRepo, number int) (giteaPR, error) {
	base, err := p.checkRepo(repo)
	if err != nil {
		return giteaPR{}, err
	}
	var pr giteaPR
	err = p.getJSON(ctx, base+"/pulls/"+strconv.Itoa(number), nil, &pr)
	return pr, err
}

func (p *Provider) fetchOne(ctx context.Context, ref ports.SCMPRRef) (ports.SCMObservation, error) {
	repo := ref.Repo
	pr, err := p.getPR(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMObservation{}, err
	}
	prObs := toPRObservation(repo, &pr)
	if u := strings.TrimSpace(ref.URL); u != "" && u != prObs.URL {
		prObs.URLAlias = u
	}
	ci, err := p.fetchCI(ctx, repo, pr.Head.SHA)
	if err != nil {
		return ports.SCMObservation{}, fmt.Errorf("gitea scm: fetch CI: %w", err)
	}
	reviews, _, err := p.listReviews(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMObservation{}, fmt.Errorf("gitea scm: fetch reviews: %w", err)
	}
	decision := reviewDecision(reviews, len(pr.RequestedReviewers) > 0)
	return ports.SCMObservation{
		Fetched:      true,
		ObservedAt:   time.Now(),
		Provider:     ProviderKey,
		Host:         p.host,
		Repo:         repo.Repo,
		PR:           prObs,
		CI:           ci,
		Review:       ports.SCMReviewObservation{Decision: string(decision)},
		Mergeability: mergeability(&pr, ci.Summary, decision),
	}, nil
}

func mergeability(pr *giteaPR, ciState string, decision domain.ReviewDecision) ports.SCMMergeabilityObservation {
	if !pr.open() {
		return ports.SCMMergeabilityObservation{State: string(domain.MergeUnknown)}
	}
	if !pr.Mergeable {
		return ports.SCMMergeabilityObservation{State: string(domain.MergeConflicting), Conflict: true, Blockers: []string{"conflicts"}}
	}
	var blockers []string
	if ciState == string(domain.CIFailing) {
		blockers = append(blockers, "ci_failing")
	}
	switch decision {
	case domain.ReviewChangesRequest:
		blockers = append(blockers, "changes_requested")
	case domain.ReviewRequired:
		blockers = append(blockers, "review_required")
	}
	if pr.isDraft() {
		blockers = append(blockers, "draft")
	}
	if len(blockers) > 0 {
		return ports.SCMMergeabilityObservation{State: string(domain.MergeBlocked), Blockers: blockers}
	}
	return ports.SCMMergeabilityObservation{State: string(domain.MergeMergeable), Mergeable: true}
}

type giteaReview struct {
	ID            int64      `json:"id"`
	User          *giteaUser `json:"user"`
	State         string     `json:"state"`
	Body          string     `json:"body"`
	CommitID      string     `json:"commit_id"`
	Dismissed     bool       `json:"dismissed"`
	CommentsCount int        `json:"comments_count"`
	HTMLURL       string     `json:"html_url"`
	SubmittedAt   *time.Time `json:"submitted_at"`
}

func (r giteaReview) login() string {
	if r.User == nil {
		return ""
	}
	return r.User.Login
}

func (p *Provider) listReviews(ctx context.Context, repo ports.SCMRepo, number int) ([]giteaReview, bool, error) {
	base, err := p.checkRepo(repo)
	if err != nil {
		return nil, false, err
	}
	var all []giteaReview
	truncated, err := paginate(ctx, p, base+"/pulls/"+strconv.Itoa(number)+"/reviews", nil, func(page []giteaReview) bool {
		all = append(all, page...)
		return true
	})
	return all, truncated, err
}

// reviewDecision folds each reviewer's latest verdict (reviews arrive oldest
// first): any change request wins, then any approval.
func reviewDecision(reviews []giteaReview, reviewersRequested bool) domain.ReviewDecision {
	latest := map[string]string{}
	for _, r := range reviews {
		state := strings.ToUpper(r.State)
		if r.Dismissed || (state != "APPROVED" && state != "REQUEST_CHANGES") {
			continue
		}
		latest[strings.ToLower(r.login())] = state
	}
	approved := false
	for _, state := range latest {
		if state == "REQUEST_CHANGES" {
			return domain.ReviewChangesRequest
		}
		approved = true
	}
	switch {
	case approved:
		return domain.ReviewApproved
	case reviewersRequested:
		return domain.ReviewRequired
	default:
		return domain.ReviewNone
	}
}

func reviewState(state string) domain.ReviewDecision {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return domain.ReviewApproved
	case "REQUEST_CHANGES":
		return domain.ReviewChangesRequest
	default:
		return domain.ReviewNone
	}
}

type giteaReviewComment struct {
	ID               int64      `json:"id"`
	Body             string     `json:"body"`
	User             *giteaUser `json:"user"`
	Resolver         *giteaUser `json:"resolver"`
	ReviewID         int64      `json:"pull_request_review_id"`
	Path             string     `json:"path"`
	Position         int        `json:"position"`
	OriginalPosition int        `json:"original_position"`
	HTMLURL          string     `json:"html_url"`
}

// FetchReviewThreads returns the review decision, review summaries and inline
// comments grouped into per-line threads.
func (p *Provider) FetchReviewThreads(ctx context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	repo := ref.Repo
	base, err := p.checkRepo(repo)
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}
	pr, err := p.getPR(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}
	reviews, partial, err := p.listReviews(ctx, repo, ref.Number)
	if err != nil {
		return ports.SCMReviewObservation{}, err
	}
	out := ports.SCMReviewObservation{Decision: string(reviewDecision(reviews, len(pr.RequestedReviewers) > 0)), Partial: partial}

	var comments []giteaReviewComment
	for _, r := range reviews {
		state := strings.ToUpper(r.State)
		if state == "PENDING" || state == "REQUEST_REVIEW" {
			continue
		}
		if state != "COMMENT" || strings.TrimSpace(r.Body) != "" {
			out.Reviews = append(out.Reviews, ports.SCMReviewSummaryObservation{
				ID: strconv.FormatInt(r.ID, 10), Author: r.login(), State: string(reviewState(state)), URL: r.HTMLURL,
				Body: r.Body, TargetSHA: r.CommitID, IsBot: isBotLogin(r.login()), SubmittedAt: safeTime(r.SubmittedAt),
			})
		}
		if r.CommentsCount == 0 {
			continue
		}
		var page []giteaReviewComment
		if err := p.getJSON(ctx, base+"/pulls/"+strconv.Itoa(ref.Number)+"/reviews/"+strconv.FormatInt(r.ID, 10)+"/comments", nil, &page); err != nil {
			return ports.SCMReviewObservation{}, err
		}
		comments = append(comments, page...)
	}
	out.Threads = groupThreads(comments)
	return out, nil
}

// groupThreads groups inline comments by file and line, since Gitea exposes
// no conversation ids. A thread's id is its first comment's id.
func groupThreads(comments []giteaReviewComment) []ports.SCMReviewThreadObservation {
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	index := map[string]int{}
	var threads []ports.SCMReviewThreadObservation
	for _, c := range comments {
		line := c.Position
		if line == 0 {
			line = c.OriginalPosition
		}
		key := c.Path + "\x00" + strconv.Itoa(line)
		i, ok := index[key]
		if !ok {
			i = len(threads)
			index[key] = i
			threads = append(threads, ports.SCMReviewThreadObservation{ID: strconv.FormatInt(c.ID, 10), Path: c.Path, Line: line, Resolved: true, IsBot: true})
		}
		th := &threads[i]
		author := ""
		if c.User != nil {
			author = c.User.Login
		}
		bot := isBotLogin(author)
		th.IsBot = th.IsBot && bot
		th.Resolved = th.Resolved && c.Resolver != nil
		if len(th.Comments) < reviewCommentLimitPerThread {
			th.Comments = append(th.Comments, ports.SCMReviewCommentObservation{
				ID: strconv.FormatInt(c.ID, 10), ReviewID: strconv.FormatInt(c.ReviewID, 10), Author: author, Body: c.Body, URL: c.HTMLURL, IsBot: bot,
			})
		}
	}
	return threads
}
