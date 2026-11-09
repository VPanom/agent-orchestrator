package session

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The Gitea failures seen in the field: a bare number against an SSH origin
// on a custom port produced a GitLab MR URL, and a /pulls/N URL was rejected.
func TestGiteaPRRefs(t *testing.T) {
	svc := NewWithDeps(Deps{GiteaBaseURL: "https://gitea.panom.se"})
	const origin = "ssh://git@gitea.panom.se:30009/owner/repo.git"
	const want = "https://gitea.panom.se/owner/repo/pulls/24"

	for _, ref := range []string{"24", "#24", want, want + "/"} {
		got, n, err := svc.normalizePRRef(ref, origin)
		if err != nil || got != want || n != 24 {
			t.Errorf("normalizePRRef(%q) = %q, %d, %v", ref, got, n, err)
		}
	}
	if err := svc.requireSameRepo(want, origin); err != nil {
		t.Errorf("requireSameRepo same repo: %v", err)
	}
	if err := svc.requireSameRepo("https://gitea.panom.se/owner/other/pulls/24", origin); !errors.Is(err, ErrProjectMismatch) {
		t.Errorf("requireSameRepo other repo = %v", err)
	}
	if err := svc.requireSameRepo(want, "https://github.com/owner/repo"); !errors.Is(err, ErrProjectMismatch) {
		t.Errorf("requireSameRepo github origin = %v", err)
	}
	repo, err := svc.scmRepoForClaim(want)
	if wantRepo := (ports.SCMRepo{Provider: "gitea", Host: "gitea.panom.se", Owner: "owner", Name: "repo", Repo: "owner/repo"}); err != nil || repo != wantRepo {
		t.Errorf("scmRepoForClaim = %+v, %v", repo, err)
	}

	sub := NewWithDeps(Deps{GiteaBaseURL: "http://host:3000/gitea/"})
	if got, _, err := sub.normalizePRRef("7", "git@host:owner/repo.git"); err != nil || got != "http://host:3000/gitea/owner/repo/pulls/7" {
		t.Errorf("sub-path normalizePRRef = %q, %v", got, err)
	}

	// Without AO_GITEA_URL, GitHub and GitLab refs behave exactly as before.
	plain := NewWithDeps(Deps{})
	got, _, err := plain.normalizePRRef("9", "https://gitlab.com/castai/ctxd")
	if err != nil || got != "https://gitlab.com/castai/ctxd/-/merge_requests/9" {
		t.Errorf("gitlab normalizePRRef = %q, %v", got, err)
	}
	if _, _, err := plain.normalizePRRef(want, origin); !errors.Is(err, ErrInvalidPRRef) {
		t.Errorf("unconfigured gitea URL err = %v", err)
	}
}
