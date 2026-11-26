# Housekeeping dry-run checkpoint

Refs #1895. This increment provides a pure policy evaluator and
`citadel housekeeping plan`, not an installed housekeeping module or cleaner.
It accepts one JSON document on stdin and emits a JSON receipt on stdout:

```
citadel housekeeping plan < inventory.json
```

There are no default roots, file arguments, shell commands, native inventory,
cache/process/Git inspection, deletion, timer installation, persistent receipt
store or remote trigger. No node configuration or ordinary agent startup is
needed. Profile admission still runs first; the HPC command list is unchanged.
Only the exact `housekeeping plan` leaf receives the startup exemption.

## Input contract

The versioned metadata input is limited to 1 MiB and 256 targets. Unknown fields,
invalid versions, duplicate target aliases, invalid states and out-of-range age
policy fail with a generic error. Target aliases are bounded ASCII identifiers;
the format intentionally has no path, command, user or remote identity fields.
Use nonidentifying aliases, not private infrastructure or customer identifiers.

This is a synthetic example, not a captured inventory. A real supplied
observation must use its own timestamps and filesystem-specific free space:

```json
{
  "version": 1,
  "targets": [{
    "id": "cache-a",
    "kind": "go_build_cache",
    "minimum_age_hours": 168,
    "keep_free_bytes": 21474836480,
    "observation": {
      "observed_at": "2030-01-02T12:00:00Z",
      "newest_entry_at": "2029-12-01T00:00:00Z",
      "estimated_bytes": 4096,
      "available_bytes": 1073741824,
      "open_handles": "no",
      "in_use": "no"
    }
  }]
}
```

Kinds are `go_build_cache`, `pnpm_store`, `npm_cache`, `uv_cache`,
`gradle_cache`, `docker_builder_cache`, `scratch` and `worktree`. Explicit
`model_cache`, `module_store`, `data_directory` and `container_images` entries
are always skipped as protected. An alias or kind declaration never proves
that an actual path belongs to that category.

States are `yes`, `no` or `unknown`; omitted states are unknown. Worktrees
additionally require `worktree_clean: yes` and `fully_pushed: yes`. These are
input assertions, not results of native verification. A later inventory adapter
must check all refs and remote reachability under its own safe Git contract;
`git status --short` alone cannot prove fully pushed work.

## Guard and receipt

Policy defaults are seven days minimum age and 20 GiB keep-free per target.
Zero policy values select those defaults; explicit age is one hour through
365 days. Unknown available bytes do not mean zero. A known zero does mean disk
pressure. A target becomes a candidate only below its keep-free threshold,
with a fresh observation no more than five minutes old, no open handles or
in-use state, sufficient age and affirmative clean/pushed assertions for a
worktree. Future observation timestamps and missing/future entry times skip.
Age uses the supplied newest entry time, not just a directory's mtime.

Every receipt says `mode: dry_run`, `source: supplied_metadata_only`,
`deletion_enabled: false` and `freed_bytes: 0`. Each target has its policy,
estimated logical bytes and one stable decision/reason. Candidate rows say
`native_revalidation_required`. Order is deterministic by alias; input is not
mutated. Estimated candidate bytes are a checked sum of supplied values, not
guaranteed physical reclaimable space: overlapping observations, hard links and
filesystem allocation can invalidate that estimate. No plan is a deletion
permit, authenticated receipt or durable proof. Do not feed it to a shell.

## Remaining integration

The existing `pruneUnreferenced` in `cmd/module_update.go` remains the separate
module-cache authority; this evaluator never calls its destructive prune path.
`status.DiskMetricsOnly` owns existing root-filesystem telemetry. A future
housekeeping inventory must measure the actual target filesystem, not infer
its free space from root-disk telemetry, and must independently establish
ownership, bounded no-follow traversal, mount boundaries, live open-handle and
in-use checks, worktree clean/pushed proof and revalidation at execution time.

Safe native inventory, run/status, execution locking and receipts, timer/update
integration, free-space recovery and authorized platform operations remain
unfinished #1895 scope. No current test or candidate receipt establishes them.

## Tests

`go test -race ./internal/housekeeping` exercises policy, unknown/unsafe input,
protected categories, freshness, per-target thresholds, worktree states and
bounded parsing with synthetic metadata. `go test -race ./cmd -run
'TestHousekeeping|TestNoSubcommandShadowsRootPersistentPreRun'` exercises stdin,
sanitized errors, output failure and the narrow offline startup boundary.
