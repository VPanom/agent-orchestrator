package domain

import (
	"fmt"
	"testing"
)

func TestGiteaRepoRef(t *testing.T) {
	const sub = "https://Git.Example.com/gitea/"
	cases := []struct {
		base, raw string
		want      string // "repo rest" or "" for no match
	}{
		{"http://gitea.lan:3000", "http://gitea.lan:3000/owner/repo.git", "owner/repo []"},
		{"http://gitea.lan:3000", "http://GITEA.lan:3000/owner/repo/issues/7", "owner/repo [issues 7]"},
		{"http://gitea.lan:3000", "git@gitea.lan:owner/repo.git", "owner/repo []"},
		{"http://gitea.lan:3000", "ssh://git@gitea.lan:2222/owner/repo.git", "owner/repo []"},
		{sub, "https://git.example.com/gitea/owner/repo.git", "owner/repo []"},
		{sub, "https://git.example.com/gitea/owner/repo/issues/3", "owner/repo [issues 3]"},
		{sub, "git@git.example.com:owner/repo.git", "owner/repo []"},
		// Sub-path must match; a sibling app on the same host is not Gitea.
		{sub, "https://git.example.com/other/owner/repo.git", ""},
		{sub, "https://git.example.com/giteax/owner/repo.git", ""},
		// HTTP host:port must match exactly.
		{"http://gitea.lan:3000", "http://gitea.lan:8080/owner/repo.git", ""},
		{"http://gitea.lan:3000", "https://github.com/owner/repo.git", ""},
		{"http://gitea.lan:3000", "git@github.com:owner/repo.git", ""},
		{"http://gitea.lan:3000", "http://gitea.lan:3000/owner", ""},
		{"", "http://gitea.lan:3000/owner/repo.git", ""},
	}
	for _, c := range cases {
		repo, rest, ok := GiteaRepoRef(c.base, c.raw)
		got := ""
		if ok {
			got = fmt.Sprintf("%s %v", repo, rest)
		}
		if got != c.want {
			t.Errorf("GiteaRepoRef(%q, %q) = %q, want %q", c.base, c.raw, got, c.want)
		}
	}
}

func TestGiteaPullRefAndURL(t *testing.T) {
	const base = "https://gitea.panom.se/sub/"
	repo, n, ok := GiteaPullRef(base, "https://gitea.panom.se/sub/owner/repo/pulls/24")
	if !ok || repo != "owner/repo" || n != 24 {
		t.Fatalf("got %q %d %v", repo, n, ok)
	}
	if got := GiteaPullURL(base, repo, n); got != "https://gitea.panom.se/sub/owner/repo/pulls/24" {
		t.Fatalf("GiteaPullURL = %q", got)
	}
	for _, raw := range []string{
		"https://gitea.panom.se/sub/owner/repo",
		"https://gitea.panom.se/sub/owner/repo/issues/3",
		"https://gitea.panom.se/sub/owner/repo/pulls/x",
		"https://gitea.panom.se/owner/repo/pulls/3",
		"https://other.example/sub/owner/repo/pulls/3",
	} {
		if _, _, ok := GiteaPullRef(base, raw); ok {
			t.Errorf("GiteaPullRef(%q) matched", raw)
		}
	}
	if _, _, ok := GiteaPullRef("", "https://gitea.panom.se/owner/repo/pulls/3"); ok {
		t.Error("matched without a configured instance")
	}
}
