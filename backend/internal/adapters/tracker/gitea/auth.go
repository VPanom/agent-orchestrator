package gitea

import (
	"context"
	"errors"
	"os"
	"strings"
)

// TokenSource yields a Gitea access token on demand. The Tracker calls Token
// once at construction (fail-fast) and again per request so rotated tokens
// are picked up without a restart.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// ErrNoToken is returned when no token source could yield a non-empty token.
var ErrNoToken = errors.New("gitea tracker: no token configured")

// StaticTokenSource is a literal token, typically used in tests.
type StaticTokenSource string

// Token returns the literal token, or ErrNoToken if it is blank.
func (s StaticTokenSource) Token(context.Context) (string, error) {
	t := strings.TrimSpace(string(s))
	if t == "" {
		return "", ErrNoToken
	}
	return t, nil
}

// EnvTokenSource reads the first non-empty value from AO_GITEA_TOKEN then
// GITEA_TOKEN, mirroring the GitLab chain's AO_-prefixed precedence. Gitea
// has no first-party CLI token store comparable to glab/gh, so there is no
// CLI fallback.
type EnvTokenSource struct{}

// Token returns the first non-empty env var, or ErrNoToken.
func (EnvTokenSource) Token(context.Context) (string, error) {
	for _, name := range []string{"AO_GITEA_TOKEN", "GITEA_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, nil
		}
	}
	return "", ErrNoToken
}
