# Harness2 on Citadel: bounded runtime proposal

Status: **PROPOSED, DESIGN REVIEW REQUIRED**. Part of [#1380](https://github.com/aceteam-ai/citadel-cli/issues/1380), related to the sovereign-agent-stack epic #1214. This document does not register a job, add an image recipe, grant execution authority, or complete the issue. No build children should be dispatched until independent design acceptance.

## Scope and evidence

The requested outcome is an `ace` command that starts one bounded Harness2 task on an explicitly selected, owned Citadel node without manual dependency installation. Propose a run-to-completion runtime, not an always-on agent service. Harness2's editable harness is task data, never authority to change Citadel, its sandbox, or platform permissions.

Research snapshot: 2026-10-22 UTC. Upstream inspected at [`google-research/harness2@1cfe254e8d25b15f37048c3730134686abe60335`](https://github.com/google-research/harness2/tree/1cfe254e8d25b15f37048c3730134686abe60335). That is an evidence pin, not an approved shipping pin. Citadel inspected at `5db1c381287e454c6d15c145783c2a8c2e7bfba5`; API and CLI observations below were checked against Aceteam `0938e9b630aaf284fb19c650e52f6220cbde0910` and ace `0dd47db1f0a6c9e52f5232eddd1b88cc8bf17a37` respectively.

| Evidence | Actual status |
| --- | --- |
| Issue, current Citadel source, upstream docs and selected source | Read, no upstream code executed |
| Measured deterministic `harness2-demo` | **NOT RUN**, execution held |
| Model-backed task on a 24 GB local node | **NOT RUN**, inference, model acquisition and native execution held |
| Sandbox escape, resource, network and cancellation acceptance | **NOT RUN** |
| New API, persistence, image, job handler or CLI | **NOT IMPLEMENTED** |

Source reading and ordinary GitHub checks cannot substitute for those measurements. In particular, the demo's deterministic editor is not proof that a local model improves a harness or can use the required tools.

## Verified upstream facts and premise corrections

1. Upstream refines prompts, tools and related harness state, not model weights. Its documented executors are OpenCode and Codex. The repository is Apache-2.0; bundled third-party components and benchmark data retain separate licensing. Do not bundle benchmark datasets by default. [README](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/README.md), [third-party notices](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/third_party/README.md).
2. Package metadata allows Python 3.12 and 3.13 and declares no mandatory Python dependencies. Benchmark setup additionally installs environments and tools. Node 20+ is documented, while OpenCode execution uses a separately pinned Bun runtime. Packaging Python plus Node alone is insufficient. [pyproject](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/pyproject.toml), [setup](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/docs/SETUP.md).
3. The current `harness2` entrypoint selects benchmark adapters, partitions and domains, and requires benchmark readiness with judge configuration. It is not an arbitrary `run <task>` interface. A custom task adapter and narrow launcher are required; passing the user's task as an upstream shell command is not an alternative. [run_sweep.py](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/cli/run_sweep.py).
4. Non-Vertex operation is plausible, not demonstrated for this integration. `validate_models` accepts provider/model identifiers and makes Vertex project checks conditional. `preflight.check` still assumes a named benchmark and installed runners. OpenCode proposer sessions generate their own configuration and isolated data directories. The build slice must install the same approved custom provider in both solver and improver contexts and replace benchmark-only preflight for custom tasks. Merely setting a base URL or removing an ADC check does not establish compatibility. [config.py](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/config.py), [preflight.py](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/preflight.py), [proposer.py](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/core/proposer.py).
5. The setup guide explicitly permits another Responses API endpoint for the Codex executor. That does not prove an OpenAI-compatible chat endpoint, Ollama, or vLLM can serve that protocol. It also does not replace the improver's model route. V1 proposes OpenCode only; Codex is a later separately tested adapter, not a fallback that silently changes API protocol. [Codex setup](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/docs/SETUP.md#codex-executor).
6. Upstream executor networking is unrestricted, and its Codex path disables the executor sandbox. Prompt guardrails and OpenCode tool permission settings do not confine hostile generated programs. An independently enforced outer sandbox is mandatory. [network warning](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/README.md#web-access).
7. `DemoAdapter` executes actual Python scripts but `DemoProposer` supplies scripted edits. Use this as an adaptation/plumbing check only. [demo.py](https://github.com/google-research/harness2/blob/1cfe254e8d25b15f37048c3730134686abe60335/harness2/demo.py).

## Proposed architecture

```text
ace authenticated command
  -> org-scoped asynchronous platform job API
  -> explicitly pinned owned node, authenticated control path
  -> Citadel admission and fenced run supervisor
  -> no-egress confined Harness2/OpenCode execution
       -> narrow job-scoped model broker only
  -> validated artifact manifest and terminal receipt
  -> authenticated status, events and artifact retrieval
```

Each arrow is an authorization boundary. None is established just by this document. Preserve existing claudecode/hermes lessons about pinned packaging, gateway routing and credential scrubbing, but do not reuse their resident instance lifecycle or mount the operator's agent home.

### Packaging and acquisition

Propose a Linux CPU-only Harness2 runner image in GHCR, addressed exclusively by reviewed digest. It includes Python 3.12, a locked dependency closure, the upstream source pin, patched OpenCode pin, its Bun runtime, and only the custom-task adapter. Node and auxiliary utilities must be pinned where actually required; exact artifact hashes, architecture support, SBOM, licenses and patch provenance belong to the later packaging review. The upstream OpenCode dependency pin is recorded in `config.DEPENDENCY_PINS`, not replaced with `latest`.

Separate image acquisition from execution. An authorized installation flow verifies the digest and pre-pulls the image, reports progress, then makes the capability available. A job never runs `harness2-setup`, installs packages, clones a repository, downloads benchmarks, or pulls weights. A missing runtime/image yields a typed refusal with remediation, not an automatic privileged installation or infinite retry. This document authorizes neither image publication nor any acquisition.

The 24 GB node requirement concerns the separately admitted local model service, not GPU access for generated agent code. The runner gets no GPU device by default. No model selection, memory capacity, engine availability or entitlement follows from a node's total VRAM alone.

### Confinement and model broker

The current generic profile, `services.RenderSandboxedModuleCompose` in `services/sandbox_profile.go`, forbids host bind mounts and outbound networking and exposes byte-oriented input/output. It also documents reachability of the bridge gateway and host services bound to all interfaces. Calling this existing profile is therefore neither sufficient proof of network isolation nor a way to add workspace mounts or model egress silently.

Propose a separately reviewed job-scoped extension that retains a non-root identity, read-only root, dropped capabilities, no privilege gain, bounded tmpfs, CPU/RAM/PID ceilings, and no host device or socket access. Inputs are copied in by a trusted supervisor as a manifest of regular files, not a broad host workspace mount. Run output is collected through a bounded byte channel. The sandbox's only model connection is a narrow broker channel with no general CONNECT, URL, file, DNS or socket forwarding. The broker is outside the generated-code trust domain. Its access topology and host firewall enforcement require native proof before capability advertisement.

The broker resolves an operator-approved endpoint grant and model ID, then performs only the required model protocol requests. It injects a job-scoped credential outside the sandbox, rejects redirects and arbitrary paths, bounds request/response sizes, model calls, tokens, concurrency and deadlines, and binds every call to org, node, run and active execution lease. It never forwards the worker's enrollment key, gateway master key, cloud ADC files or the operator's personal API keys. Generated code may consume its bounded model budget, so broker authorization must assume hostile callers.

Sovereign local engines and hosted models are distinct grants. Resolve local engine access through trusted node configuration and the gateway, not a caller-supplied endpoint or arbitrary mesh address. Private/loopback model routes require explicit machine-local authority and must not be enabled by globally relaxing public-endpoint SSRF policy. Hosted use needs explicit cost/entitlement consent. If either solver or improver is unsupported, refuse before execution rather than switch providers or leak tasks to a hosted fallback.

No host networking, host Docker/Podman socket, privileged container, SSH agent, home directory, inherited environment, or arbitrary host shell. Use an allowlisted environment; do not load the host workspace's `.env`. Harness plugins and scripts remain confined run data and cannot install host dependencies or alter the broker configuration. WorkBuddy-Bench and nested Docker are **out of scope for v1**.

### Proposed job contract

`HARNESS2_RUN` is a proposed new type, not registered. A strict versioned schema should reject unknown fields and unbounded collections. The following fields are design inputs, not a wire contract approved for producers:

| Field | Intended meaning |
| --- | --- |
| `schemaVersion` | Exact accepted contract version |
| `task` | Bounded UTF-8 task text or an authorized staged task reference |
| `inputs` | Immutable staged regular-file manifest, relative paths, size and digest |
| `executor` | `opencode` only in v1 |
| `modelGrant` | Opaque approved model/endpoint selection, never a secret or URL |
| `k`, `mode` | Bounded recursion budget and `parallel` or `sequential` |
| `libraryId`, `expectedRevision` | Owner-scoped library plus optimistic concurrency precondition |
| `limits` | Policy-lowered wall time, calls, tokens, bytes, files and resource limits |
| `requestId` | Canonical idempotency identity bound to a canonical request digest |

Org, actor, node authorization and execution lease come from authenticated control metadata, not trusted claims in `task` or caller-supplied paths. Outputs are assigned by the supervisor, never an arbitrary host destination. Reject absolute paths, traversal, symlinks, hardlinks, special files, overlapping aliases and expansions escaping the owned job directory. Input quotas apply before copying; output quotas apply while writing and again at collection. No unrestricted archives or host-file tasks in v1.

Proposed conservative initial policy, requiring ratification: one task per job, `k` from 1 to 4, default 1; default sequential mode; at most two candidate executions at once even in parallel mode. Every solver, improver, selector and lesson-generation call shares one aggregate budget. Exact call/token, task-byte, file-count, disk, CPU, RAM, duration and kill-grace values must be fixed from measured acceptance before advertising the capability. Unspecified limits are refusal, not unlimited execution. Parallel recursion is not a free independent worker lane.

### Admission, cancellation and restart

Use the existing heavy-lane declaration, long watchdog tier and cooperative cancellation interfaces as integration points, not as evidence of completed admission. Recheck node/org permission, image digest, confinement enforcement, staged inputs, disk quota, endpoint grant and aggregate model budget before launch. Claiming the node job does not reserve GPU memory; local model admission must also succeed without violating #1271's queue contract or #1340's per-service admission/drain/custody gates. No implicit eviction or preemption of running work.

The supervisor owns an execution identity, lease and exclusive workspace/library writer lock. Cancellation first prevents new broker calls and child launches, requests stop, then terminates and, after a bounded grace, kills the entire container/process tree. A request to stop is not terminal cancellation. Verify actual exit and containment cleanup before publishing terminal state, releasing locks/resources or admitting a conflicting writer. If exit cannot be proved, quarantine the run and hold custody. Hard deadlines stay fatal even if a child later writes output.

Persist state transitions and terminal artifacts before acknowledgement. A client disconnect does not cancel accepted work; reconnect reads authenticated status. A restarted worker must not blindly rerun an uncertain model session or adopt partial harness edits. It reconciles the execution identity, terminal journal and active lease; if authority or process custody is uncertain it remains non-executable until resolved. Duplicate delivery returns the committed receipt only under the accepted recovery policy, not a new run.

This depends on #1222/#1227's finished-result recovery and fencing work. Existing generic dispatch registration is not proof that all of claim, completion, terminal acknowledgement and restart are wired for this runtime. Source-only staging can precede those dependencies, but no recoverable production claim or runtime activation can. Replacement credentials inherit no old execution/receipt authority; same-key original-token recovery must obey the separately accepted exact-identity, read-only contract. No direct-Redis credential shortcut.

### Persistence and output custody

The trusted supervisor derives separate directories under its configured owned workspace by org, actor, library and run ID. Directory creation, traversal and publication need anchored filesystem operations and explicit owner/permission policy, not string concatenation alone. The sandbox gets neither the parent library nor another user's runs.

Each run consumes an immutable library revision and writes a new candidate revision. After actual exit, validate output paths, types, quotas and content digests, then publish a manifest and terminal receipt atomically. Adopted harness and lessons may become the next default within that same owner-scoped library by compare-and-swap, never by overwriting an unrelated current revision. Preserve the previous revision and evidence for rollback. Concurrent revisions conflict explicitly. A failed, cancelled, timed-out or uncertain run does not advance the library.

Treat generated harnesses, scripts, plugins and lessons as untrusted artifacts, not verified instructions. Adoption grants no broader network, filesystem, credential or execution scope on the next task. Do not automatically import them into the user's personal agent home, shared org memory or AceTeam skills. Memory synchronization is a later explicit consent and tenancy design.

The terminal manifest should identify request/run identity, input and library digests, runtime/upstream/patch digests, executor and exact model selection, applied limits, aggregate usage, terminal state and output size/hash entries. Raw task text, private files, bearer credentials and model traces stay out of generic logs, activity streams, error messages and pub/sub progress. Private transcripts, if retained, require owner-scoped authenticated retrieval and a documented retention/deletion policy. Remote artifact storage and storage quotas remain unselected; do not invent a durable storage guarantee from node-local files.

## CLI and platform API proposal

Propose `ace harness2 run <task>` with optional node/model selectors, recursion budget and mode. The node must resolve unambiguously to an owned authorized target; otherwise require explicit selection. Discovery exposes only a verified Harness2 capability and approved endpoint/model grants, not provider secrets. Additional status, events, cancel and artifact commands use the returned run ID; cancellation needs authenticated authority and reports requested versus confirmed termination separately.

Use new authenticated org-scoped **Next.js** routes, provisionally:

| Route | Proposed operation |
| --- | --- |
| `POST /api/fabric/harness2/runs` | Validate/stage request, enforce policy, accept idempotently and return 202 plus run ID |
| `GET /api/fabric/harness2/runs/{runId}` | Authorized durable state and terminal receipt |
| `GET /api/fabric/harness2/runs/{runId}/events` | Bounded nonsecret progress with reconnect cursor |
| `POST /api/fabric/harness2/runs/{runId}/cancel` | Idempotent cancellation request, not an immediate terminal success |
| `GET /api/fabric/harness2/runs/{runId}/artifacts/{artifactId}` | Authorized bounded retrieval of a published manifest entry |

These routes do not exist today and their names need backend owner acceptance. Inputs require an authenticated bounded staging operation before dispatch; do not put large file blobs on the stream. Existing `POST /api/fabric/dispatches` registers an identity/digest/token record; it is not a task-upload/enqueue endpoint. The current Python `POST /fabric/dispatch` has a bounded synchronous result wait, so increasing its timeout is not the long-running solution. The CLI must not call the Python backend directly, accept a user-selected Redis queue, or treat a timed-out synchronous call as permission to launch a duplicate.

The ace repository's current `src/commands/fabric.ts` supplies login/discover/status primitives, not this command. A cross-repo backend and CLI contract review is required. Reuse protected dispatcher routing, membership/permission checks and the authenticated API/WS worker control path after verifying the relevant recovery hooks. No new public model/agent endpoint or anonymous status stream.

## Proposed review and build sequence

These are **candidate slices only**, not assigned issues or an accepted Design of Record. After independent design review and owner acceptance, file properly prefixed children with explicit prerequisite links; only unblocked, approved slices receive `automated`.

1. **H1, contracts and offline fixtures:** ratify job/API/artifact/limits schemas and custom-task adapter semantics. Golden tests cover tenancy, unknown fields, budgets, paths, duplicate IDs and unsupported endpoints. No executor/model call.
2. **H2, confined task adapter and packaging:** lock source/dependency/patch provenance; construct the runner without benchmark bootstrap. Solver and improver use the same approved provider seam. Add deterministic fake executor/broker tests. Image build/publication requires its own permission and reviewed CI.
3. **H3, supervisor and model-broker confinement:** implement scoped byte transport, process/container custody, quotas and restricted endpoint access. Independent security review plus Linux Docker/rootless Podman proof, including bridge-gateway escape attempts, is required. No capability advertisement on unproved runtimes.
4. **H4, node handler and durable lifecycle:** depend on H1/H3 and the accepted P0 recovery/fencing integration. Implement heavy admission, job control, terminal journal, original-token recovery and revision publication; prove no overlapping writers across restart, cancellation and redelivery.
5. **H5, Aceteam asynchronous API:** accepted backend-owned authorization/staging/status/events/cancel/artifact design, then source implementation with strict ownership and grant checks. Depends on H1 and the relevant recovery protocol. No SQL application or rollout is included in this design.
6. **H6, ace command:** depend on H5's accepted contract and H4's capability. Add fake-transport tests for submission, disconnect/reconnect, errors, cancellation and hash-verified artifact fetch. Never silently send local tasks to hosted models.
7. **H7, separately authorized acceptance:** record demo, local model and hostile-workload measurements on named disposable hosts, then review whether distribution/activation is safe. This is an execution gate, not a source-only child with fabricated completion evidence.

## Acceptance plan and blocking decisions

Offline suites should cover custom-task adapter compatibility, malicious file manifests, library revision conflicts, inherited-environment refusal, strict endpoint grants, model-call accounting, complete process termination, late output rejection, cancelled adoption, corrupt terminal state and duplicate delivery. Use fakes until local execution scope is explicitly granted. Ordinary existing GitHub CI does not approve native execution.

When separately authorized, record exact image/upstream/adapter/model digests, hardware/OS/runtime, all limits, input digest, wall time, peak CPU/RSS/VRAM/disk, model calls/tokens, observed terminal state, artifacts and independent review. Run the deterministic demo without network; verify both recursion modes and persisted lessons. Then run a benign bounded task against an explicitly approved local model on the 24 GB node, disconnect the client and restart the worker at controlled boundaries. Separately test cancellation with child processes, deliberate quota exhaustion, bridge/host/network probing and concurrent library requests. Acceptance must show actual exited custody and finished-result recovery, not just a prompt asking the agent to stop.

Decisions required from automated-citadel and automated-aceteam before build dispatch:

- Accept the **custom-task, OpenCode-only, no-WorkBuddy** v1 scope, or choose an explicitly benchmark-only first deliverable instead.
- Accept ownership and enforcement topology for the job-scoped sandbox extension and model broker, with native test criteria for host-service reachability.
- Ratify endpoint/provider compatibility, model/tool capabilities, aggregate limits and hosted-cost consent. No particular local model is approved by this document.
- Assign backend ownership and approve the asynchronous API, staging, retention and P0 lifecycle dependencies. Specify the durable storage tier before promising results beyond node-local retention.
- Authorize a separate bounded demo/native/model test window if the issue's measured acceptance is to proceed. Until then both requested runs remain **NOT RUN** and #1380 stays open/WIP.
