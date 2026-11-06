// Package gitea implements the ports.Tracker outbound port for Gitea Issues
// on a single, configured (typically self-hosted) Gitea instance. v1 is
// read-only:
//
//   - Get returns a normalized snapshot of one issue
//     (GET /api/v1/repos/{owner}/{repo}/issues/{index}).
//   - List returns a filtered slice of a repo's issues, paginated by page
//     number with state/labels/assignee filters. Pull requests are excluded
//     (type=issues, plus a client-side pull_request guard), and the assignee
//     filter is also enforced client-side for older Gitea versions.
//   - Preflight performs a single GET /api/v1/user to verify the token; a
//     success is cached for the lifetime of the Tracker, failures are not.
//
// The instance base URL (scheme, host, optional port and sub-path) comes from
// AO_GITEA_URL; the token from AO_GITEA_TOKEN / GITEA_TOKEN. TrackerID.Host
// "" means the configured instance; any other host must match it exactly or
// the call is rejected before a credential is attached.
//
// # State mapping
//
//   - open   -> open
//   - closed -> done
package gitea
