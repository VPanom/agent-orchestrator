package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/httpkit"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	defaultUserAgent   = "ao-gitea-scm/1"
	defaultHTTPTimeout = 30 * time.Second
	// Gitea's default MAX_RESPONSE_ITEMS is 50; asking for more is clamped.
	pageSize = 50
	// Bounds pagination so a server that never returns a short page cannot
	// loop forever (50 pages * 50 = 2.5k items).
	maxPages = 50
	// maxBodyBytes caps a single response body.
	maxBodyBytes = 16 << 20
)

var (
	// ErrNotFound is returned when a Gitea API resource does not exist.
	ErrNotFound = ports.ErrSCMNotFound
	// ErrRateLimited is returned when Gitea responds with HTTP 429.
	ErrRateLimited = errors.New("gitea scm: rate limited")
	// ErrAuthFailed is returned when Gitea rejects the token (401/403).
	ErrAuthFailed = errors.New("gitea scm: authentication failed")
	// ErrHostNotAllowed is returned for a repo on a host other than the
	// configured instance, before any credential is attached.
	ErrHostNotAllowed = errors.New("gitea scm: host does not match configured instance")
)

// RateLimitError carries Retry-After / reset hints from a 429 so the SCM
// observer can apply a per-provider cooldown.
type RateLimitError struct{ *httpkit.RateLimitError }

// GetRetryAfter exposes the Retry-After hint to the observer.
func (e RateLimitError) GetRetryAfter() time.Duration { return e.RetryAfter }

// GetResetAt exposes the reset hint to the observer.
func (e RateLimitError) GetResetAt() time.Time { return e.ResetAt }

// Unwrap lets errors.Is match ErrRateLimited.
func (e RateLimitError) Unwrap() error { return e.RateLimitError }

// HTTPError is a non-2xx response that is not otherwise classified.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("gitea scm: HTTP %d: %s", e.Status, e.Message)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends one API request. path is relative to <base>/api/v1 and must
// already be escaped; body, when non-nil, is sent as JSON.
func (p *Provider) do(ctx context.Context, method, path string, q url.Values, body any) (response, error) {
	u := p.apiBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{}, fmt.Errorf("gitea scm: encode %s body: %w", path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return response{}, fmt.Errorf("gitea scm: build request: %w", err)
	}
	tok, err := p.tokens.Token(ctx)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("gitea scm: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return response{status: resp.StatusCode}, fmt.Errorf("gitea scm: read %s: %w", path, err)
	}
	out := response{status: resp.StatusCode, header: resp.Header, body: raw}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return out, nil
	}
	msg := httpkit.Message(raw)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return out, fmt.Errorf("%w: gitea %s: %s", ErrNotFound, path, msg)
	case http.StatusTooManyRequests:
		return out, RateLimitError{httpkit.BuildRateLimitError(resp, msg, ErrRateLimited)}
	case http.StatusUnauthorized, http.StatusForbidden:
		return out, fmt.Errorf("%w: %s", ErrAuthFailed, msg)
	}
	return out, &HTTPError{Status: resp.StatusCode, Message: msg}
}

func (p *Provider) getJSON(ctx context.Context, path string, q url.Values, dst any) error {
	resp, err := p.do(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.body, dst); err != nil {
		return fmt.Errorf("gitea scm: decode %s: %w", path, err)
	}
	return nil
}

// paginate requests pages by number (a Link header built from a ROOT_URL that
// differs from AO_GITEA_URL cannot misroute it) and decodes each into a []T.
// visit returns false to stop early. truncated reports the page cap was hit.
func paginate[T any](ctx context.Context, p *Provider, path string, q url.Values, visit func([]T) bool) (truncated bool, err error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("limit", strconv.Itoa(pageSize))
	seen := 0
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		resp, err := p.do(ctx, http.MethodGet, path, q, nil)
		if err != nil {
			return false, err
		}
		var items []T
		if err := json.Unmarshal(resp.body, &items); err != nil {
			return false, fmt.Errorf("gitea scm: decode %s: %w", path, err)
		}
		seen += len(items)
		// An empty page ends the list; a short page does not, since an admin
		// may set MAX_RESPONSE_ITEMS below our requested limit.
		if len(items) == 0 || !visit(items) {
			return false, nil
		}
		if total, err := strconv.Atoi(resp.header.Get("X-Total-Count")); err == nil && seen >= total {
			return false, nil
		}
	}
	return true, nil
}
