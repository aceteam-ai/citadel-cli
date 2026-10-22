// Package memorygit provides the inactive, read-only MEMORY_GIT_V1 source
// companion for aceteam-ai/aceteam issue 11997.
//
// # Wire contract
//
// A job payload has exactly two string fields: request (a JSON Request) and
// passcode (attached by the platform, never an MCP argument). The request's
// version is 1 and node_id must equal the configured node identity. Scope is
// an opaque key into the node's allowlist of memory directories. There is no
// request operand for repository paths, commands, executables or environment.
//
// Resolve accepts revision. History accepts name and an explicit limit from
// 1 to 100. Diff accepts name, from and to. Export accepts name and revision
// and returns one regular committed blob as base64, including its byte count
// and resolved commit SHA. Export writes no file or archive. Revisions accept
// 7 to 40 hex SHA characters, HEAD, or HEAD~1 through HEAD~99. Slugs accept
// ASCII letters, digits, underscores and hyphens, with a letter, digit or
// underscore first, and up to 252 bytes before the .md suffix. The platform
// adapter must apply its canonical slug policy before constructing Request.
//
// Responses are JSON Result values. Each child's stdout is limited to 256 KiB;
// overflow fails atomically with output_limit. Base64 and JSON escaping expand
// the wire result, so an adapter must allow a bounded encoded result (2 MiB is
// sufficient), then enforce these decoded byte/entry bounds while parsing.
// Requests are limited to 4096 bytes and the whole operation to 15 seconds,
// bounded further by the incoming job context. Child stderr is discarded.
//
// # Required platform and node integration, not implemented here
//
// Before dispatch the platform must call resolve_memory_scope with the actual
// authenticated actor and preserve the scope refusals from the existing scope
// slice. It must not map org/global access onto a legacy personal directory.
// It must select an exact node that advertises MEMORY_GIT_V1 from its current
// handler set and fail closed when absent, stale or ambiguous. It must never
// retry through SHELL_COMMAND, sh, a local shell, or another node.
//
// The adapter must call attach_shell_credential using the authenticated acting
// org, selected node and acting user, exactly as the existing shell path does.
// It must not accept a passcode from an MCP argument. The node handler reuses
// ShellGateReason with live Shell permission, strict passcode State, and the
// same node-wide throttle/vault lockout as shell. This core does not verify
// the platform actor or escrow binding itself; existing transport and the
// future adapter retain that responsibility. Scope directories are trusted
// node configuration, not authority supplied by a request.
//
// The first runner supports Linux only and fails closed elsewhere. It requires
// a node-owned repository with a private .git directory and no metadata
// symlinks, hardlinks, special files, linked worktrees, alternate object roots
// or group/other writable metadata. The bounded metadata check permits at most
// 16384 entries and depth 32. It pins .git by an inherited descriptor and uses
// fixed argv with exec.CommandContext, an isolated environment and no working
// tree. Repository creation/import/migration, mutations/rollback, synchronization,
// bulk journal/filesystem export, registration, advertisement and rollout are
// separate reviewed work. Existing platform draft slices remain unchanged.
package memorygit
