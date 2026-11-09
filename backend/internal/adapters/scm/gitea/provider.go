package gitea

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	trackergitea "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/gitea"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ProviderKey is the SCMRepo.Provider value this adapter owns.
const ProviderKey = "gitea"

// TokenSource yields the Gitea access token. It is shared with the Gitea
// tracker so AO_GITEA_TOKEN / GITEA_TOKEN resolve the same way.
type TokenSource = trackergitea.TokenSource

// EnvTokenSource reads AO_GITEA_TOKEN then GITEA_TOKEN.
type EnvTokenSource = trackergitea.EnvTokenSource

// StaticTokenSource is a literal token, typically used in tests.
type StaticTokenSource = trackergitea.StaticTokenSource

// ErrNoToken is returned when no token is configured.
var ErrNoToken = trackergitea.ErrNoToken

// ErrBadBaseURL is returned for a malformed AO_GITEA_URL.
var ErrBadBaseURL = errors.New("gitea scm: invalid base URL")

// ProviderOptions configures the Gitea SCM provider. BaseURL and Token are
// required.
type ProviderOptions struct {
	// BaseURL is the instance root as opened in a browser, e.g.
	// "https://gitea.example.com" or "http://host:3000/gitea".
	BaseURL    string
	Token      TokenSource
	HTTPClient *http.Client
	UserAgent  string
	Logger     *slog.Logger
}

// Provider implements the provider-neutral SCM interfaces for one Gitea
// instance.
type Provider struct {
	baseURL   string // scheme://host[:port][/sub-path], no trailing slash
	apiBase   string // baseURL + /api/v1
	host      string // lowercased host[:port] of baseURL
	tokens    TokenSource
	http      *http.Client
	userAgent string
	logger    *slog.Logger

	identityMu sync.Mutex
	identity   *ports.SCMIdentity
}

// NewProvider validates the base URL. The token is resolved lazily per
// request so a missing token does not block daemon startup;
// SCMCredentialsAvailable reports it instead.
func NewProvider(opts ProviderOptions) (*Provider, error) {
	if opts.Token == nil {
		return nil, ErrNoToken
	}
	u, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%w: %q (want http(s)://host[:port][/path])", ErrBadBaseURL, opts.BaseURL)
	}
	base := u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
	p := &Provider{
		baseURL:   base,
		apiBase:   base + "/api/v1",
		host:      strings.ToLower(u.Host),
		tokens:    opts.Token,
		http:      opts.HTTPClient,
		userAgent: opts.UserAgent,
		logger:    opts.Logger,
	}
	if p.http == nil {
		p.http = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if p.userAgent == "" {
		p.userAgent = defaultUserAgent
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p, nil
}

// BaseURL returns the normalized instance root (no trailing slash).
func (p *Provider) BaseURL() string { return p.baseURL }

// ParseRepository accepts http(s), ssh:// and scp-like remotes that point at
// the configured instance (see domain.GiteaRepoRef). Any other host returns
// false so the token never leaves its instance.
func (p *Provider) ParseRepository(remote string) (ports.SCMRepo, bool) {
	repo, rest, ok := domain.GiteaRepoRef(p.baseURL, remote)
	if !ok || len(rest) != 0 {
		return ports.SCMRepo{}, false
	}
	owner, name, _ := strings.Cut(repo, "/")
	return ports.SCMRepo{Provider: ProviderKey, Host: p.host, Owner: owner, Name: name, Repo: repo}, true
}

// checkRepo rejects repos on any host other than the configured instance and
// returns the escaped "owner/repo" API path segment.
func (p *Provider) checkRepo(repo ports.SCMRepo) (string, error) {
	if host := strings.ToLower(strings.TrimSpace(repo.Host)); host != "" && host != p.host {
		return "", fmt.Errorf("%w: %q (configured %q)", ErrHostNotAllowed, repo.Host, p.host)
	}
	if strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" || strings.Contains(repo.Name, "/") {
		return "", fmt.Errorf("gitea scm: invalid repository %q", repo.Owner+"/"+repo.Name)
	}
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name), nil
}

// AuthenticatedIdentity returns the account behind the token, cached for the
// provider's lifetime once resolved.
func (p *Provider) AuthenticatedIdentity(ctx context.Context) (ports.SCMIdentity, error) {
	p.identityMu.Lock()
	defer p.identityMu.Unlock()
	if p.identity != nil {
		return *p.identity, nil
	}
	var user giteaUser
	if err := p.getJSON(ctx, "/user", nil, &user); err != nil {
		return ports.SCMIdentity{}, err
	}
	login := strings.TrimSpace(user.Login)
	ident := ports.SCMIdentity{Login: login, Human: login != "" && !isBotLogin(login)}
	p.identity = &ident
	return ident, nil
}

// AuthenticatedIdentityForHost is the host-scoped form used by the multi
// provider; only the configured instance is accepted.
func (p *Provider) AuthenticatedIdentityForHost(ctx context.Context, host string) (ports.SCMIdentity, error) {
	if h := strings.ToLower(strings.TrimSpace(host)); h != "" && h != p.host {
		return ports.SCMIdentity{}, fmt.Errorf("%w: %q", ErrHostNotAllowed, host)
	}
	return p.AuthenticatedIdentity(ctx)
}

// SCMCredentialsAvailable reports whether a Gitea token is configured.
func (p *Provider) SCMCredentialsAvailable(ctx context.Context) (bool, error) {
	if _, err := p.tokens.Token(ctx); err != nil {
		if errors.Is(err, ErrNoToken) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isBotLogin(login string) bool {
	l := strings.ToLower(login)
	return strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") || l == "gitea-actions" || l == "ghost"
}
