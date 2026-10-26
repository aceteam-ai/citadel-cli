# Current-key device binding display

Citadel #1466 consumes the read-only endpoint from Aceteam #12443, implementing
Jason's D1–D3 decisions on #12328. It suppresses the saved-link warning only for
one verified observation. It does not gate workers, job claims, Control Center
actions or enrollment. There is no local proof marker, persisted confirmation,
grant repair or credential replacement.

## One request and independent comparison inputs

The client captures the selected bearer, saved organization, API origin/base
path and control-plane setting, plus the numeric Headscale ID from live mesh
status. A hostname, IP, fabric node ID, saved diagnostic identity or earlier
registration result cannot substitute for live mesh identity. Missing identity
keeps the warning without sending the lookup.

GET /api/fabric/device-auth/current-binding has no query, body or caller-selected
identity parameters. It sends only the current bearer, accepts JSON, requests
no-store and refuses redirects. A dedicated fresh HTTP/1 transport avoids
reused-connection replay and platform-transport refusal-body inspection. The
client performs no retry. The complete observation has a five-second context
deadline, including input capture and rechecks; HTTP also has a five-second cap.

Only a bounded 200 application/json response with no-store may be decoded.
Confirmed requires exactly state, org_id, node and observed_at; node requires
exactly domain and id. The domain must be headscale; the canonical UUID org and
positive decimal-string node ID must match the independent captured inputs.
The client bounds IDs to 128 bytes; it does not assert a server uint64 ceiling.
Not-linked requires exactly state and observed_at and never suppresses warning.
Unknown, duplicate, case-mismatched, extra, missing, null, wrongly typed or
trailing fields, HTML and responses larger than 4096 bytes remain unknown.

observed_at must have the server's UTC millisecond shape, but is informational.
No acceptance decision compares it with the host wall clock or starts a cached
freshness interval from it. After HTTP, current credential digests, org, origin,
base path, control-plane setting and live mesh identity are rechecked. Any
missing, changed, cancelled or over-budget input keeps the warning.

This is the accepted stateless comparison, not an all-writer transaction or
durable generation/ABA guarantee. The broader local credential-record design
remains separate. A confirmation belongs only to its producing render/startup
decision and is discarded afterwards.

## Fail-closed display and invocation sites

401/403 retain an unverifiable warning without reading the refusal body or
guessing which authorization check failed. The guidance explains that a
credential predating server verification needs fresh browser enrollment, while
missing permissions or membership can also prevent verification. Per D1,
existing credentials receive no scope/path backfill and keep the warning until
re-enrollment. 404/405 report absent server capability. 429, 5xx, redirects,
transport errors and malformed observations stay unknown. No response bodies,
credentials, endpoint values or identity values are added to diagnostic output.

The check runs once for a standard/JSON status invocation after its existing
network probe, the initial
interactive status render, worker startup after mesh verification, Control
Center's initial reconnect display, and explicit reconnect. Existing explicit
init/login/enroll diagnostics can also make this single observation when live
identity is present. It is absent from heartbeat and retry loops.

Interactive status refreshes use the offline collector: they neither repeat
the lookup nor retain the initial render's confirmation, so uncertainty is
shown again on later refreshes. A new explicit status invocation obtains its
own observation. Servers, including sovereign installations, without the
capability or usable Headscale evidence remain unverified.

## Verification boundary

Fake fixtures cover strict server responses, status-only/unread refusals,
base-path and bearer routing, redirects, cancellation, request bounds, late
input changes, unavailable mesh identity, replacement/noninheritance, repeated
invocations and read-only preferred/fallback credentials. Tests use private
HOME/XDG/profile directories and inert dependency functions or local HTTP
fixtures. No real node, key, service, API or database is used.

Local candidate execution remains held; normal GitHub CI provides execution
evidence for the eventual draft head. The paired server's independent review
must settle its wire contract before the client push. Source review/CI do not
prove deployment, live fleet acceptance, server availability or native safety.
