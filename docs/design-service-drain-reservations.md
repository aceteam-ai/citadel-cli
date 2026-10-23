# Per-service admission, drain and GPU reservations (#1340)

Status: **design proposal only; mechanism UNACCEPTED**. No runtime change,
reservation record, service action, new refusal policy or build child is created
by this document. Parent #1335; source base
`dc139d1de01239fc4d432990bef415d59f6b4e33`.

## Settled policy and full scope

Founder policy is not reopened: **every non-pinned service may be
evicted**, including Ollama, vLLM and OmniVoice. Pinned services are never
evicted. Already admitted work **must drain naturally; eviction never cancels
it**. Idle is at most a candidate-ordering hint, not an eligibility gate or proof
of no active work. A low-utilization sample is not a drain certificate.

When an image-model variant cannot fit, #1340 requests a job-scoped reservation
using existing lifecycle infrastructure, reports genuine pending preparation as
`model_warming`, and restores the peers it displaced on the accepted release
boundary. This is not satisfied by permanently refusing busy services, making
Ollama ineligible, or implementing only an idle-only subset. The required
mechanism, lifecycle ownership and ingress protocol still need owner review.

## Observed source, not a claim that the mechanism already exists

| Existing surface at the base above | Evidence and limitation |
| --- | --- |
| `internal/jobs/reservation.go:256–323`, `Reserve` | Plans, persists eviction tags/prior desired status and directly stops each selected service. No per-service admission closure or active-call drain precedes stop. Partial reservations survive errors and require cleanup. |
| `internal/status/preempt.go:20–26,49–64` | Pinned is an exclusion; idle sorts candidates only. This preserves founder policy, but candidate selection is not permission to interrupt busy work. |
| `internal/jobs/model_exclusivity.go:185–272`, `ReserveExclusive` | Existing explicit whole-card path stops every non-pinned candidate, even without a measured footprint. It is not a job-scoped drain implementation. |
| `internal/jobs/reservation.go:448–505` | Release visits matching job tags, starts services, restores prior durable status, then clears tags. It has no new media-process/allocator release certificate. |
| `internal/jobs/reservation.go:508–552` | Startup reconciliation assumes its worker lock proves previous job ownership ended; comments explicitly call out CLI/MCP ownership and surviving fine-tune processes. A resident media sidecar can also survive a worker. |
| `internal/worker/gpu_tracker.go:5–10,86–99` | Tracks boolean GPU slots, not service/request leases; explicitly excludes MEDIA_GENERATE and several sidecars. |
| `internal/worker/runner.go:257–280,978+` | ActiveJobs includes pending/dispatching and the requesting job itself. Node-wide Drain permanently stops fetching; it is not a reversible per-service fence. |
| `internal/gateway/chat_route.go:190–274` | Resolves/swaps then reverse-proxies streams to a runtime; request-activity recording is not a native-work lease. vLLM can use a validated non-loopback node address. |
| `cmd/mcp_local.go:441,638–715` | Local chat reaches the runtime directly. Local-exclusive operations resolve service handlers and reserve/release outside the worker's ownership model. |
| `internal/jobs/service_handler.go:796–820,1276+` | StopServiceByName reduces a typed result to an error, not proof of exact instance exit; freeVRAMBytes aggregates devices. Neither is sufficient for a selected-device grant. |

Merged #1339 (`576734e422e61e4587b7c6f0082a5a3fb930467d`) already adds useful
**image-manager** inference leases: `image_swap.py:318–325,384–388` counts and
releases inflight users; `:491–510` stops outgoing leases and drains before its
own swap. `app.py:721–735` retains the pipeline through generation and drops its
references before lease release. Do not repeat the older claim that these locks
cover only construction. They still do not establish a cross-service fence;
video has a separate resident pipeline (`app.py:750–759`). Historical #1339 GPU
acceptance is not acceptance of this new eviction protocol.

## Proposed owner and identity model

Go owns authenticated requesting-job identity, common admission, service
lifecycle authority, persistence/recovery, reporting and peer restoration. The
sidecar owns exact model/profile state, inference/load leases, unload and actual
native-work observations. It receives no Docker socket or right to kill arbitrary
GPU processes. Resource ownership and source-delivery completion are distinct.

A proposed reservation identity binds node/install identity, unpredictable
operation ID, job/attempt, owner kind (worker/CLI/MCP), owner incarnation, monotonically
fenced epoch, exact target GPU UUID/partition, target service incarnation and
reviewed model/profile digest. Each displaced service record binds logical ID
to authoritative runtime/container identity, configuration generation, pin
generation, original observed running state, original desired status and the
specific stop/unload authority. A name, PID, bearer token in a log or old
`evicted_by_job` string alone is insufficient. Exact schema/storage and upgrade
interoperability with legacy tags remain unaccepted.

Cooperating control participants reject stale epochs and late acknowledgements.
Only the current owner may advance or release its operation. Node/worker locks
serialize relevant control updates, not long drain waits or native execution.
Respect existing canonical-node and fine-tune safety lock ordering; do not
recursively call a reservation primitive while holding its own filesystem lock.
Derive and test the composed lock order before implementation rather than assume
one global mutex makes independent services safe.

## All-ingress admission is the prerequisite, not an observation heuristic

Admission must atomically check the service/incarnation epoch and acquire an
active-call lease **before** dispatch, GPU-slot acquisition or runtime allocation.
Closing a service's admission prevents later calls from sneaking in while its
already admitted calls finish. A lease covers runtime queueing, streaming and
native work; an HTTP disconnect/deadline or worker callback return does not
automatically release it. Completion requires a correlated service acknowledgement
or authoritative exact-instance termination after work has naturally drained.

Coverage must be enumerated and tested, not inferred from one gateway hook:

- worker LLM handlers and sidecar jobs, plus preparation/model-loading paths;
- gateway chat/completions/embedding/borrow routes and streaming requests;
- local MCP/CLI chat and model control, mesh/runtime clients and other Citadel
  processes on the host;
- direct engine API clients, runtime-internal queued work, external/vendor-managed
  runtimes, and any unmanaged process sharing the selected GPU.

A proxy-only counter cannot cover bypass clients. Propose an enrolled service
adapter that owns admission at the runtime boundary, or a reviewed transport
topology that actually forces every caller through that boundary. Which adapters
and network/credential changes achieve this is an owner/cross-repo question.
Unknown ingress remains an explicit unresolved coverage gap, **not** proof of
idleness and **not** a newly authorized permanent exclusion/refusal policy for
non-pinned services. Do not claim full #1340 delivery until the gap is resolved.
Externally managed services need explicit enrolled lifecycle authority; the
founder eviction policy does not authorize killing unrelated processes by name.

## Proposed reservation state machine

```text
requested -> fencing -> draining -> stopping -> granted
   |             |          |           |          |
   +-------------+----------+-----------+----------+-> releasing
                                                       -> restoring -> released
unknown native/owner/stop outcome -> custody_unknown -> explicit reconciliation
```

1. **Request/plan.** Validate existing job permissions/deadline and the requested
   model variant's bounded selected-device profile. A sidecar resource outcome
   is advisory; Go chooses lifecycle candidates using pinned policy and existing
   planning concepts. Bind to one GPU, not summed free memory across devices.
   Repeated requests for the same operation are idempotent, not new evictions.
2. **Fence.** Persist the proposed owner/epoch and close admission for the exact
   selected service incarnations. Coordinate CLI/MCP starts, auto-restart,
   gateway warming, swaps and reconcile so none can allocate around the hold.
   Changing global desired state is not itself an admission fence.
3. **Drain.** Wait for those services' existing active-call leases and internal
   queues to finish, never cancel them. Do not hold a GPU slot, generation mutex
   or lifecycle lock they need to finish. Never wait for global ActiveJobs==0
   from the requesting image job: it waits for itself and unrelated workloads.
4. **Revalidate/stop.** Recheck pins, ownership, configuration and incarnation at
   the lifecycle serialization point immediately before every stop/unload. A
   service pinned during drain must not be evicted. Replan and release only our
   obsolete fences; a changed or uncertain identity is not a stop target.
   Candidate estimates do not replace exact managed-service stop acknowledgement,
   process/container exit or an enrolled model-unload release certificate.
5. **Grant.** Verify actual release and fresh selected-device free capacity with
   headroom before allowing the target model to load/execute. A nil stop error,
   disappearance from one status sample, low utilization, empty HTTP queue or
   estimated reclaimed bytes is not sufficient. Report uncertainty honestly.
6. **Release/restore.** Only after the target's native work and GPU residency are
   released, restore services actually displaced by this owner. Preserve original
   state and current operator intent; verify readiness per exact restored
   incarnation before reopening its admission and clearing recoverable records.

Pin checks must compose with every supported pin/manifest writer. A second
by-name check without a common serialization/epoch boundary has a race. How to
detect and handle out-of-band manifest edits remains explicit coverage, not a
claim that arbitrary host writes can be atomically fenced.

## Model residency: finished job is not released VRAM

A successful image response can leave an image pipeline warm, and the video
pipeline, background load or offloaded CPU state can outlive the request. Do not
call Release in a job defer and immediately restart peers over those allocations.
Review one of these ownership choices before implementation:

- job-scoped immediate release: close target admission, finish its owned work,
  unload all reservation-owned allocations and verify selected-device release;
- bounded residency lease after job completion: transfer to an explicitly durable
  service-incarnation owner, account for all joined users, then perform the same
  drain/unload/release protocol when its reviewed idle expiry or other demand wins.

Idle expiry triggers reconciliation, not proof of exit. Exact idle policy,
profile estimate/headroom, multi-GPU placement and image/video ownership are
unaccepted. Returning a receipt is neither a warm-residency lease nor permission
to hold peers stopped indefinitely. Later LLM/Ollama demand must participate in
the same protocol to reclaim media allocations; coordination is bidirectional.

## Failure, cancellation, recovery and original state

Before any stop, a requester timeout/cancellation can withdraw its own pending
reservation and reopen only its owned fences after verifying epochs. It cannot
cancel another service's admitted work. After a partial stop, persist each exact
outcome and reconcile only affected owned entries. If target work has started,
the requesting job's own reviewed cancellation policy applies; uncertain native
exit retains target/resource custody. A drain timeout never turns into a forced
peer cancellation. Watchdog, lost response, missing worker or stale heartbeat
alone cannot authorize release or peer restart.

Recovery must distinguish live CLI/MCP owners, surviving sidecars and old worker
epochs before interpreting legacy tags as orphaned. Recovered owners require
fresh fenced handshakes; stale owners cannot resume allocation or restore the
same service. Preserve fine-tune safety holds. Do not retrofit a second ledger
that silently disagrees with the current manifest tags: schema/migration,
atomicity and recovery of every partial durable write need independent review.

Restore only the runtime/container instances this reservation was authorized to
control, using the captured configuration and prior running/desired state.
Never start a service that was already stopped, restore a replaced definition,
clear a different owner's tag or override an intervening operator stop. Restore
failure remains visible and retryable under retained ownership; no orphaned
entry is forgotten merely to make the operation look complete. Exact unload vs
service stop and which processes an adapter may control need per-engine review.

Preserve source-job custody and waiters. Reservation waiting must not discard an
already claimed job, clear its delivery token, ACK it early or create a second
execution. This does not change #1271's two admitted jobs plus one non-executable
source claim or introduce queue displacement/deferral. Any needed capacity/retry
protocol is a separate accepted prerequisite; do not smuggle one into a warming
response. P0 #1222/#1227 completion/receipt/restart semantics remain independently
gated and must compose without rerunning a finished image after lost delivery.

## Health and paired caller contract

Propose bounded reservation phase/owner-incarnation/epoch, target model/profile,
selected device, affected service IDs, active lease counts and restore outcomes
in health and the job's typed result. No prompt, credential, raw request or
arbitrary error body is forwarded. Health performs no load/stop/restore and
separates liveness, admission closed, draining, model preparation, granted,
custody unknown and restore failure.

`model_warming` is appropriate only while a real owned reservation/preparation is
pending. Unknown stop, missing coverage, expired demand or terminal resource
failure must not become endless warming or false hot readiness. Freeze any new
reason/result fields, retry/deadline handling and original-token correlation
with automated-aceteam before activation; existing HTTP503 handling alone is
not a coordinated protocol. No new cross-repo endpoint is implemented here.

## Owner questions and document-only proposed children

Route through automated-citadel and automated-aceteam; mechanism decisions remain
with the owner. Do not dispatch builds or file child issues from this proposal:

1. Ratify all-ingress coverage/enrollment for each engine, including direct and
   externally managed clients; who owns runtime admission acknowledgements?
2. Ratify owner/epoch/service/GPU identities, persistent state and legacy-tag
   interoperability across worker, CLI/MCP, startup and fine-tune holds.
3. Choose immediate job release vs bounded warm-residency ownership, exact
   drain/reconciliation deadlines and partial-failure/restore semantics.
4. Ratify lifecycle adapters: exact allowed stop vs model-unload operations,
   actual-work and release evidence, pin/config race serialization and restoration.
5. Freeze paired warming/status/capacity semantics without changing #1271 or P0
   delivery authority. Then define bounded source children and hermetic fake-
   adapter/state-machine/failure/mutation tests with distinguishing assertions.

Native acceptance required by #1340 is **NOT RUN / held**:

- a pinned service is never evicted, including pin changes during drain;
- an unpinned service with in-flight worker/gateway/direct enrolled work drains
  naturally before stop, with no late ingress or cancelled peer request;
- success, failure and requester timeout restore the exact displaced services
  after actual target release, or retain truthful custody when release is unknown.

Under separate authority, record exact service/model/container/GPU identities,
baseline/original state, natural completion evidence, reclaimed VRAM and cold/
warm/restore timing; exercise restart and partial failures without losing work.
Historical #1339 timings or mocked tests are not those measurements. No local
candidate execution/tests/formatter/build, runtime/GPU probe, reservation/live
write, Docker/image/model/download/inference, service action, SQL/deployment,
undraft or merge is authorized. Normal existing CI on this docs-only draft is
allowed; source review and cross-compilation cannot establish native drain proof.
