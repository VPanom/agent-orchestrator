package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	_ ports.SCMMerger          = (*Provider)(nil)
	_ ports.SCMReviewRequester = (*Provider)(nil)
	_ ports.SCMReviewPoster    = (*Provider)(nil)
)

var headSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

func (p *Provider) pullPath(pr ports.SCMPRRef) (string, error) {
	if pr.Number <= 0 {
		return "", fmt.Errorf("gitea scm: invalid pull request number %d", pr.Number)
	}
	base, err := p.checkRepo(pr.Repo)
	if err != nil {
		return "", err
	}
	return base + "/pulls/" + strconv.Itoa(pr.Number), nil
}

func statusErr(resp response, err error) error {
	var httpErr *HTTPError
	switch {
	case !errors.As(err, &httpErr):
		return err
	case resp.status == http.StatusConflict:
		return fmt.Errorf("%w: %w", ports.ErrSCMHeadChanged, err)
	case resp.status == http.StatusMethodNotAllowed || resp.status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", ports.ErrSCMNotMergeable, err)
	}
	return err
}

// MergePullRequest squash-merges a PR guarded by the reviewed head SHA:
// Gitea's head_commit_id rejects the merge with 409 if the head moved.
func (p *Provider) MergePullRequest(ctx context.Context, request ports.SCMMergeRequest) (ports.SCMMergeResult, error) {
	if request.Method != ports.SCMMergeSquash {
		return ports.SCMMergeResult{}, fmt.Errorf("gitea scm: unsupported merge method %q", request.Method)
	}
	head := strings.TrimSpace(request.ExpectedHeadSHA)
	if !headSHAPattern.MatchString(head) {
		return ports.SCMMergeResult{}, fmt.Errorf("gitea scm: invalid expected head sha")
	}
	path, err := p.pullPath(request.PR)
	if err != nil {
		return ports.SCMMergeResult{}, err
	}
	resp, err := p.do(ctx, http.MethodPost, path+"/merge", nil, map[string]any{"Do": "squash", "head_commit_id": head})
	if err != nil {
		return ports.SCMMergeResult{}, statusErr(resp, err)
	}
	// The merge endpoint returns an empty body; read the commit off the PR.
	pr, err := p.getPR(ctx, request.PR.Repo, request.PR.Number)
	if err != nil {
		return ports.SCMMergeResult{}, fmt.Errorf("gitea scm: read merged pull request: %w", err)
	}
	if pr.MergeCommitSHA == "" {
		return ports.SCMMergeResult{}, fmt.Errorf("gitea scm: merged pull request has no merge commit sha")
	}
	return ports.SCMMergeResult{MergeCommitSHA: pr.MergeCommitSHA}, nil
}

// RequestReview adds a user to the PR's requested reviewers.
func (p *Provider) RequestReview(ctx context.Context, request ports.SCMReviewRequest) error {
	reviewer := strings.TrimSpace(strings.TrimPrefix(request.Reviewer, "@"))
	if reviewer == "" {
		return fmt.Errorf("gitea scm: reviewer is required")
	}
	path, err := p.pullPath(request.PR)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, http.MethodPost, path+"/requested_reviewers", nil, map[string]any{"reviewers": []string{reviewer}})
	return err
}

// PostReview posts a comment-only review with inline comments and returns
// its id. COMMENT is used because reviews post from the PR author's own
// account, and Gitea rejects approving or rejecting your own PR.
func (p *Provider) PostReview(ctx context.Context, request ports.SCMReviewPostRequest) (string, error) {
	path, err := p.pullPath(request.PR)
	if err != nil {
		return "", err
	}
	type inline struct {
		Path        string `json:"path"`
		Body        string `json:"body"`
		NewPosition int    `json:"new_position"`
	}
	comments := make([]inline, 0, len(request.Comments))
	for _, c := range request.Comments {
		if strings.TrimSpace(c.Path) == "" || c.Line <= 0 || strings.TrimSpace(c.Body) == "" {
			return "", fmt.Errorf("gitea scm: review comment needs path, positive line and body")
		}
		comments = append(comments, inline{Path: c.Path, Body: c.Body, NewPosition: c.Line})
	}
	body := map[string]any{"event": "COMMENT", "body": request.Body, "comments": comments}
	if sha := strings.TrimSpace(request.CommitSHA); sha != "" {
		body["commit_id"] = sha
	}
	resp, err := p.do(ctx, http.MethodPost, path+"/reviews", nil, body)
	if err != nil {
		return "", err
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(resp.body, &created); err != nil || created.ID <= 0 {
		return "", fmt.Errorf("gitea scm: review response missing id")
	}
	return strconv.FormatInt(created.ID, 10), nil
}
