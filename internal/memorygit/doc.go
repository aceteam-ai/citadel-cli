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
// # Strict construction follow-up (#1436, partial)
//
// jobs.NewMemoryGitHandler requires a nonnil Engine, live Shell permission
// callback, strict State callback and the configured node-wide shared throttle.
// Its fields are private and nil/zero/incomplete handlers refuse execution.
// Construction does not evaluate live callbacks; each dispatch still applies
// ShellGateReason's existing runtime refusal order. Legacy passcode callbacks
// cannot be supplied to this constructor. Neither the generic shell gate nor
// existing shell/desktop/workflow throttle behavior is changed.
//
// A nonnil throttle establishes only dependency presence. The node's future
// wiring must supply the actual shared configured object; this constructor
// cannot prove pointer identity across consumers or effective tuning. Existing
// nonpositive/disabled throttle tuning semantics are not changed or certified.
// The fixtures use NewPasscodeThrottle with positive defaults and exercise
// failure-budget sharing through actual shell and memory-git handlers.
//
// This is not activation or completion of #1436. The future transport/producer
// contract must settle metadata keys such as target_node, timeout_ms and rayId;
// they remain rejected, not silently stripped or accepted here. The adapter
// must preserve the server-attached passcode until this handler checks it,
// using the existing plain NewLegacyHandlerAdapter shape, not a desktop-style
// passcodeGate that deletes the field. No adapter, registration or capability
// advertisement is added by this follow-up; the unregistered fixture remains.
// Actual platform node identity, authenticated scope selection and exact-node
// credential attachment still need their separately reviewed integration.
//
// Repository provisioning also remains separate: node ownership/private .git
// mode, clone/import hardlinks, repacking, metadata-walk bounds and an accepted
// maintenance/GC policy must be addressed before activation. This slice does
// not change repository policy, Git configuration, argv, environment or runner.
// Local candidate tests, builds and native acceptance have not been run for
// this follow-up; existing GitHub CI is separate evidence, not activation.
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
