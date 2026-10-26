# Worker platform-call recovery

Ref #1476; depends on #1477's rotating platform transport. This backstop covers
an API worker that keeps polling but receives wrong answers, so the existing
no-poll STALL check never fires. It does not change the old STALL/STUCK policy.

## Actual HTTP evidence

The transport emits timestamped, typed observations for its configured platform
origin. Job consume, heartbeat HTTP fallback, desired-state and other Redis API
calls share one worker health tracker. Terminal token refresh feeds that same
tracker. An actual 2xx records platform_ok_at and clears failure escalation.
Method-level nil returns, cached token validation, WebSocket handshakes/writes
and successful local queue operations do not fabricate HTTP success.

A 404, 5xx or non-platform error body records a wrong answer. Existing bounded
body observation and diagnostic privacy remain: no extra reads, credentials,
URLs or bodies are added to health state or logs. A retired pool's late wrong
response cannot prove that a fresh pool failed. Older response timestamps do
not move the failure clock backwards; delayed good/non-outage evidence breaks
an overlapping streak conservatively.

DNS, connect and network timeout failures clear escalation as cannot_connect.
Caller cancellation adds no platform observation. Authorization 401/403 and
quota 429 clear escalation; canonical validation errors and redirects also
clear it. These conditions are not repaired by restarting the worker.

## Recovery timing and admission fence

WORKER_SELF_HEAL_PLATFORM_FAIL_SECONDS defaults to 900. A nonpositive value
disables only this check; WORKER_SELF_HEAL=false disables the whole monitor.

1. A full window of fresh wrong-answer responses requests a fresh transport
   pool and closes idle connections on the retired pool. Active HTTP requests
   may finish; no request is replayed. This also bypasses busy HTTP/2 reuse.
2. Another full window of wrong responses after that action arms restart
   backoff. A single old failed response followed by silence earns neither
   window.
3. Backoff is uniformly jittered from one through five minutes per monitor.
   A fresh wrong response at/after its deadline is required before exit.

At the default threshold, a fresh process must earn at least 31 minutes of
failure evidence before another automatic restart. No persisted timer or
host-state file carries restart authority across processes. This bounds
restart churn during a real platform incident and spreads fleet retries.

In-flight/executing jobs, draining and startup grace suspend escalation and
require a new idle failure window. Before exit, the runner temporarily fences
pickup, cancels/waits for the current poll/claim handoff, then checks admitted
jobs, pending dispatch and other drain owners under the admission mutex. A
late-admitted/pending job or another drain vetoes exit and resets escalation.
Actual health evidence is checked again inside that final fence, so a success
or offline/refusal response after the monitor decision also vetoes exit. The new
process-exit path also requires the same systemd INVOCATION_ID evidence used
by remote restart; foreground workers can re-dial without exiting into an
unmanaged dead process. Existing service Restart=on-failure handles exit 1.
launchd does not set INVOCATION_ID: macOS workers therefore only re-dial and
fail safe without a platform-triggered process exit. This does not add launchd
restart support or alter the existing STALL/STUCK policy.

The local /worker endpoint and heartbeat worker block add platform_ok_at,
platform_failing_since, platform_last_outcome and platform_last_http_status.
Times are absent until their evidence exists. A failure timestamp reports
connectivity state even while a legitimate job suppresses recovery actions.

## Remote restart provenance and proof boundary

The existing backend citadel_worker_restart tool validates organization/node
ownership, then sends POST /agent/worker-restart through its VPN HTTP client.
Source: aceteam python-backend/routes/aceteam_mcp_citadel_agent.py,
_agent_request and citadel_worker_restart. It does not dispatch a queue job or
reuse the node's outbound HTTP pool. The Citadel status route accepts the
existing VPN-origin gate without performing outbound token validation; other
callers retain the existing token authorization requirement.

Tests establish a failing outbound fixture, exercise that mesh route with an
unavailable token validator, and verify the non-mesh refusal. The actual
buildAgentProviders restart callback is separately tested against the failed
outbound client, using an inert processExiter and service-manager fixture.
The acknowledgement precedes the scheduled non-zero exit. No live restart,
backend database call, mesh connection or fleet drill was performed.

A later separately authorized staging drill can close the deployment proof:

1. Select an idle disposable service-managed node in the test organization;
   verify its worker status through the backend MCP and preserve inbound mesh
   reachability. Use the staging account's controlled API fault fixture to
   return wrong answers after the worker has linked successfully.
2. Confirm platform_failing_since is present while HTTP success stops, and call
   citadel_worker_restart through that backend. Record its ok/restarting/
   process-exit acknowledgement.
3. Restore the fixture and verify a new process start time plus recovered
   platform_ok_at and job polling through the agent tools.

The backend, ownership data, relay and inbound mesh must remain available for
that control path. Source/fixture tests prove independence from the node's
outbound pool, not availability of a down backend or a broken mesh.

## Test custody

Fake-clock policy tests cover both fresh windows, bounded backoff, new-process
rearming, success/offline/auth/quota resets, clock rollback, suspension and the
idle guard. HTTP fixtures cover actual status/envelope evidence, fallback and
WebSocket provenance, token refresh and retired-pool observations. New tests
use private HOME/XDG directories; none read real node configuration.

Pure policy and file-selected monitor tests passed locally in isolated
HOME/XDG before the execution-custody approval rejection. Further candidate
execution is held; HTTP/runner/status/cmd integration tests await normal GitHub
CI. No local full cmd/worker suite or native/service execution is claimed.
