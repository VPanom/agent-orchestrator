// Package gitea observes Gitea pull requests for AO's SCM integrations.
//
// It implements the provider-neutral scm.Provider interface against the Gitea
// REST API v1 for one configured instance (AO_GITEA_URL, which may carry a
// sub-path such as https://host/gitea). Credentials come from AO_GITEA_TOKEN or
// GITEA_TOKEN, shared with the Gitea issue tracker, and are only ever sent to
// that instance: a remote on any other host is not parsed as a Gitea repo.
//
// # Repository matching
//
// ParseRepository delegates to domain.GiteaRepoRef: http(s) remotes must match
// the instance host:port and sub-path; SSH remotes match on hostname only,
// because Gitea's SSH port differs from its HTTP port. Every parsed repo uses
// the instance's HTTP host as SCMRepo.Host, so PR rows from an SSH checkout and
// from a pasted web URL agree.
//
// # State mapping
//
//   - PR state: merged → merged; closed → closed; open + draft (or a "WIP:" /
//     "[WIP]" title prefix on instances without the draft flag) → draft;
//     otherwise open.
//   - CI: the combined commit status of the head SHA, which is where Gitea
//     Actions and external CI report. success and warning → passing; failure
//     and error → failing; pending → pending; no statuses → unknown.
//   - Review: the latest APPROVED / REQUEST_CHANGES review per reviewer
//     (dismissed reviews ignored). Any change request wins, then any approval;
//     outstanding requested reviewers → review_required; otherwise none.
//   - Mergeability: mergeable=false → conflicting; mergeable=true is then
//     blocked by failing CI, requested changes, required review, or draft.
//
// Gitea has no review-thread API, so review comments are grouped into threads
// by file and line, and a thread is resolved when every comment has a
// resolver. Threads cannot be resolved through the API.
package gitea
