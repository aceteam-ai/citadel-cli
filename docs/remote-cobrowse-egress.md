# Remote co-browse egress guard (LO3)

Refs #1672, #1668 and aceteam-ai/aceteam#14040 (LD3).

The node-wide `COBROWSE_SESSION` manager requires a loopback HTTP proxy before
launching any remote browser, including throwaway and saved-profile sessions.
The proxy is shared code with local mode (`internal/browseregress`), not a second
address-policy implementation. The URL parser and address helpers live in
`internal/browserurl`; local API names remain compatibility wrappers.

## Enforcement

Initial and explicit agent/human navigation URLs are checked before launch or
navigation. More importantly, the browser's own traffic uses the proxy on every
new connection. The proxy resolves the destination itself, examines all answers,
and dials an approved numeric address. It never hands the original name to a
second dial-time resolver. Redirect destinations, frames, script requests,
subresources and CONNECT tunnels pass through the same check. Reusing an existing
public socket does not change that socket's destination when DNS changes.
Plain HTTP also rechecks current URL and DNS policy before forwarding each
request, including over a pooled upstream connection. A current private,
mixed-address or metadata answer refuses that request without reaching the
existing public socket. New dials still resolve and pin independently; CONNECT
checks the tunnel destination when it is opened.

Loopback, link-local, metadata and other reserved addresses are refused. Explicit
metadata endpoints in private ranges are hard refusals too, including the
[AWS IPv6 endpoint](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html)
and [Alibaba endpoint](https://www.alibabacloud.com/help/en/ecs/user-guide/view-instance-metadata/).
RFC1918, CGNAT, ULA and non-public names receive an approval-required refusal.
They cannot be dialed: O5 has not supplied a node-verifiable private-network grant
contract, and this slice invents no approval token, credential or bypass flag.
A response from a mixed public/private DNS answer set is refused before any dial.

Remote launch options always use the session's own guard, overriding any inherited
proxy option. The existing browser argument builder disables the implicit
loopback bypass and non-proxied WebRTC UDP. The proxy itself never uses an
HTTP_PROXY environment route. The browser is stopped before its guard closes;
a stop racing a late launch also closes the late-created guard. A stopped proxy
cannot accept a late CONNECT tunnel into its tracked set.

The job's `capabilities` action advertises `features: ["egress_guard"]`. A running
session advertises that feature only while its guard is active. The additive
`egress_private_holds` counter makes proxy-level private refusals visible on
session status. Old consumers may ignore both fields. Status remains answerable;
agent actions, `InspectTarget`, handoff/resume and viewer attachment fail closed
when the required guard is absent or inactive.

## Existing sessions end at upgrade

The proxy belongs to the worker process. A Chromium process already running
without it cannot be reconfigured safely in place. A Chromium process pointing
at a previous worker's proxy cannot be adopted as though that proxy still exists.

On restart or upgrade, guarded remote sessions are stopped and must start again
through the guarded path. At recovery, old session records are expired with
`ended_reason: egress_guard_restart_required` and an instruction to start a new
session. PID start identity and process role are checked before signaling stale
children. Expiring a record also revokes its old PID-only orphan-sweep claim,
so a fallback sweep cannot bypass the creation-time fence. Records belonging to a different live worker retain their existing
owner protection. Persistent profiles are sealed before an orderly restart.
This replaces remote-browser continuity across worker restart for this slice.
A durable proxy sidecar and its custody protocol are outside LO3.

### Legacy COBROWSE start/actions refused

The separate legacy `COBROWSE` job family without `session_id` cannot start,
navigate, capture or otherwise drive its unguarded singleton. It returns explicit
restart guidance: call `cobrowse_start` for a guarded `COBROWSE_SESSION`, then use
the returned `session_id`. The handler does not silently invent an ID, retarget
action calls, reuse an old grant or launch a browser. Legacy `status` and `stop`
remain available for observation and cleanup, including while handed off.
An orderly worker self-restart always stops its known legacy singleton too.

Aceteam main still has callers that omit `session_id` (source check recorded on
#1672). They will receive this intentional refusal until they start and address
a new session. The paired backend guidance and rollout ordering need independent
review before release. No process is killed merely for listening on the old
browser debug port: identity-unverified legacy orphans require operator cleanup.

## Verification and release

Local validation uses fake launchers, fake CDP, fake process tables and loopback
HTTP fixtures under isolated HOME/XDG with external networking blocked. It covers
remote dispatch, redirects, rebinding, subresources, both session profile types,
proxy failure, interrupted launch, restart and legacy recovery. No local native
browser, live node, service restart or deployment is required.

The source PR is security-gated. Opus review and Jason's head-pinned decision
precede release through the existing Citadel release process. Real Linux and
Windows browser acceptance belongs to the operator; source fixtures do not
certify native browser proxy behavior.
