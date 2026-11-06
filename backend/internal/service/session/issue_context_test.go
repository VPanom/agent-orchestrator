package session

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCanonicalGitLabIssueURL(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantNative string
		wantHost   string
		wantOk     bool
	}{
		{
			name:       "gitlab.com simple repo",
			raw:        "https://gitlab.com/group/project/-/issues/42",
			wantNative: "group/project#42",
			wantHost:   "", // gitlab.com → zero value
			wantOk:     true,
		},
		{
			name:       "gitlab.com nested namespace",
			raw:        "https://gitlab.com/group/subgroup/project/-/issues/42",
			wantNative: "group/subgroup/project#42",
			wantHost:   "", // gitlab.com → zero value
			wantOk:     true,
		},
		{
			name:       "www.gitlab.com normalizes to empty host",
			raw:        "https://www.gitlab.com/group/project/-/issues/42",
			wantNative: "group/project#42",
			wantHost:   "", // www.gitlab.com → zero value (same as gitlab.com)
			wantOk:     true,
		},
		{
			name:       "self-managed GitLab",
			raw:        "https://gitlab.internal/group/project/-/issues/42",
			wantNative: "group/project#42",
			wantHost:   "gitlab.internal",
			wantOk:     true,
		},
		{
			name:       "self-managed GitLab nested namespace",
			raw:        "https://gitlab.internal/group/subgroup/project/-/issues/99",
			wantNative: "group/subgroup/project#99",
			wantHost:   "gitlab.internal",
			wantOk:     true,
		},
		{
			name:       "self-managed with port",
			raw:        "https://gitlab.local:8443/group/project/-/issues/7",
			wantNative: "group/project#7",
			wantHost:   "gitlab.local:8443",
			wantOk:     true,
		},
		{
			name:       "not a GitLab issue URL",
			raw:        "https://github.com/owner/repo/issues/42",
			wantNative: "",
			wantHost:   "",
			wantOk:     false,
		},
		{
			name:       "missing issue number",
			raw:        "https://gitlab.com/group/project/-/issues/",
			wantNative: "",
			wantHost:   "",
			wantOk:     false,
		},
		{
			name:       "no project path",
			raw:        "https://gitlab.com/-/issues/42",
			wantNative: "",
			wantHost:   "",
			wantOk:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNative, gotHost, gotOk := canonicalGitLabIssueURL(tt.raw)
			if gotOk != tt.wantOk {
				t.Fatalf("ok = %v, want %v", gotOk, tt.wantOk)
			}
			if gotNative != tt.wantNative {
				t.Errorf("native = %q, want %q", gotNative, tt.wantNative)
			}
			if gotHost != tt.wantHost {
				t.Errorf("host = %q, want %q", gotHost, tt.wantHost)
			}
		})
	}
}

// staticSCM is a minimal scmProvider stub that returns a canned SCMRepo for
// any remote. It lets trackerIDForIssue tests exercise the SCM-backed
// repoForTracker path without a full provider stack.
type staticSCM struct {
	repo ports.SCMRepo
}

func (s staticSCM) ParseRepository(string) (ports.SCMRepo, bool) {
	if s.repo.Provider == "" || s.repo.Repo == "" {
		return ports.SCMRepo{}, false
	}
	return s.repo, true
}
func (staticSCM) FetchPullRequests(context.Context, []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	return nil, nil
}
func (staticSCM) FetchReviewThreads(context.Context, ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	return ports.SCMReviewObservation{}, nil
}

func TestTrackerIDForIssue_PlainNumberSelfManagedGitLab(t *testing.T) {
	svc := NewWithDeps(Deps{SCM: staticSCM{repo: ports.SCMRepo{
		Provider: "gitlab",
		Host:     "gitlab.internal",
		Owner:    "group",
		Name:     "project",
		Repo:     "group/project",
	}}})

	id, ok := svc.trackerIDForIssue(ports.SpawnConfig{IssueID: "42"}, domain.ProjectRecord{
		RepoOriginURL: "https://gitlab.internal/group/project.git",
	})
	if !ok {
		t.Fatal("trackerIDForIssue ok = false")
	}
	if id.Provider != domain.TrackerProviderGitLab {
		t.Errorf("Provider = %q, want gitlab", id.Provider)
	}
	if id.Host != "gitlab.internal" {
		t.Errorf("Host = %q, want gitlab.internal", id.Host)
	}
	if id.Native != "group/project#42" {
		t.Errorf("Native = %q, want group/project#42", id.Native)
	}
}

func TestTrackerIDForIssue_PlainNumberGitLabDotCom(t *testing.T) {
	svc := NewWithDeps(Deps{SCM: staticSCM{repo: ports.SCMRepo{
		Provider: "gitlab",
		Host:     "gitlab.com",
		Owner:    "group",
		Name:     "project",
		Repo:     "group/project",
	}}})

	id, ok := svc.trackerIDForIssue(ports.SpawnConfig{IssueID: "42"}, domain.ProjectRecord{
		RepoOriginURL: "https://gitlab.com/group/project.git",
	})
	if !ok {
		t.Fatal("trackerIDForIssue ok = false")
	}
	if id.Provider != domain.TrackerProviderGitLab {
		t.Errorf("Provider = %q, want gitlab", id.Provider)
	}
	if id.Host != "" {
		t.Errorf("Host = %q, want \"\" (gitlab.com zero value)", id.Host)
	}
	if id.Native != "group/project#42" {
		t.Errorf("Native = %q, want group/project#42", id.Native)
	}
}

func TestTrackerIDForIssue_PlainNumberGitHub(t *testing.T) {
	svc := NewWithDeps(Deps{SCM: staticSCM{repo: ports.SCMRepo{
		Provider: "github",
		Host:     "github.com",
		Owner:    "owner",
		Name:     "repo",
		Repo:     "owner/repo",
	}}})

	id, ok := svc.trackerIDForIssue(ports.SpawnConfig{IssueID: "42"}, domain.ProjectRecord{
		RepoOriginURL: "https://github.com/owner/repo.git",
	})
	if !ok {
		t.Fatal("trackerIDForIssue ok = false")
	}
	if id.Provider != domain.TrackerProviderGitHub {
		t.Errorf("Provider = %q, want github", id.Provider)
	}
	if id.Host != "" {
		t.Errorf("Host = %q, want \"\" (GitHub never uses Host)", id.Host)
	}
	if id.Native != "owner/repo#42" {
		t.Errorf("Native = %q, want owner/repo#42", id.Native)
	}
}

// A Gitea origin is not claimed by any SCM sub-provider, so the
// --tracker-provider hint decides; Host stays "" (= configured instance).
func TestTrackerIDForIssue_GiteaHint(t *testing.T) {
	svc := NewWithDeps(Deps{})
	project := domain.ProjectRecord{RepoOriginURL: "http://gitea.lan:3000/owner/repo.git"}
	for _, issue := range []domain.IssueID{"42", "owner/repo#42"} {
		id, ok := svc.trackerIDForIssue(ports.SpawnConfig{IssueID: issue, TrackerProvider: domain.TrackerProviderGitea}, project)
		if !ok {
			t.Fatalf("%s: trackerIDForIssue ok = false", issue)
		}
		want := domain.TrackerID{Provider: domain.TrackerProviderGitea, Native: "owner/repo#42"}
		if id != want {
			t.Errorf("%s: id = %+v, want %+v", issue, id, want)
		}
	}
}

// With AO_GITEA_URL configured, a Gitea origin (even under a sub-path) and
// Gitea issue URLs resolve to gitea without any --tracker-provider hint.
func TestTrackerIDForIssue_ConfiguredGitea(t *testing.T) {
	svc := NewWithDeps(Deps{GiteaBaseURL: "https://git.example.com/gitea/"})
	giteaOrigin := domain.ProjectRecord{RepoOriginURL: "https://git.example.com/gitea/owner/repo.git"}
	githubOrigin := domain.ProjectRecord{RepoOriginURL: "https://github.com/gh/repo.git"}
	want := domain.TrackerID{Provider: domain.TrackerProviderGitea, Native: "owner/repo#42"}

	cases := []struct {
		issue   domain.IssueID
		project domain.ProjectRecord
	}{
		{"42", giteaOrigin},            // sub-path stripped from origin
		{"owner/repo#42", giteaOrigin}, // native form on a Gitea origin
		{"https://git.example.com/gitea/owner/repo/issues/42", githubOrigin}, // URL wins
	}
	for _, c := range cases {
		id, ok := svc.trackerIDForIssue(ports.SpawnConfig{IssueID: c.issue, TrackerProvider: domain.TrackerProviderGitHub}, c.project)
		if !ok || id != want {
			t.Errorf("%s: id = %+v, %v; want %+v", c.issue, id, ok, want)
		}
	}

	// URLs on other hosts or sub-paths are not Gitea issues.
	for _, raw := range []domain.IssueID{
		"https://git.example.com/other/owner/repo/issues/42",
		"https://git.example.com/gitea/owner/repo/pulls/42",
	} {
		if id, ok := svc.trackerIDForIssue(ports.SpawnConfig{IssueID: raw}, githubOrigin); ok && id.Provider == domain.TrackerProviderGitea {
			t.Errorf("%s: resolved to gitea %+v", raw, id)
		}
	}
}
