# Platform API connection recovery

Ref #1475. The platform's DNS target can move while its old edge still accepts
TCP/TLS and returns errors. An HTTP error does not itself end a keep-alive
connection, and an active HTTP/2 connection can keep accepting streams.

The internal/platformhttp package supplies the shared transport policy:

- A pool generation has a maximum age of ten minutes. Before admitting a
  request after that age, the helper replaces the whole transport and calls
  CloseIdleConnections on the retired one. New requests therefore dial again,
  including when an HTTP/2 connection in the old pool is still busy.
- Idle connections expire after ninety seconds, preserving a shorter explicit
  idle timeout. TLS settings, proxy/dial policy and HTTP protocol settings are
  copied from the original transport. Existing in-flight requests may finish;
  the helper does not replay or cancel them.
- Three consecutive qualifying failures must span at least two distinct request
  paths in the configured API origin. Query changes do not create distinct
  endpoints. A qualifying failure is a 404, 5xx, non-platform error body or
  network failure unless the caller context was cancelled. The first qualifying
  streak can retire the pool immediately; subsequent failure rotations are at
  least sixty seconds apart, across pool generations. An outage on one endpoint
  therefore cannot rotate the pool, and a wider incident cannot cause a TLS
  handshake storm. Counters saturate at the threshold while cooling down;
  another qualifying failure after the window can trigger recovery.
  Successful responses reset both count and path history. Late responses from
  retired generations cannot change the replacement's counters.
  Secondary/redirect origins do not contribute. Maximum-age rotation is
  independent of the failure cooldown.
- Other error responses are observed only through bytes the caller reads, up
  to 4097 bytes. A nonempty API error string, or JSON-RPC error code/message,
  identifies a platform error. Other complete bodies are an upstream mismatch;
  incomplete or over-limit opaque samples are conservatively unclassified.
  Both contribute to the same failure threshold and cooldown. A 404 is counted
  once when the caller reads/closes its body, using one mismatch reason if
  applicable (an unread/empty body still counts as a 404). A 5xx is counted at
  headers and needs no body observation. Every response contributes at most once.
- The transport does not read or retain 401/403 bodies. It does not inspect
  successful binary, protobuf or SSE bodies, alter rate-limit hints, introduce
  retries, follow redirects independently, or change auth and job policy.

Connection and recovery logs contain only the actual TCP peer IP and a closed
reason, such as max_connection_age, http_404, network_failure,
upstream mismatch or unclassified_error_body. With a configured proxy, the
TCP peer may be that proxy. Logs never include URLs, queries, headers, bodies
or arbitrary network/server error text. Fresh-dial metadata is opt-in through
Options.Debugf (the Redis API client uses its existing debug callback), with
no default normal/TUI output. Rotation diagnostics remain operator-visible.

The helper is used by the Redis API client (heartbeat/status, consume, pub/sub,
actual/desired state), nexus/device auth and auth-key renewal, terminal token
validators, team chat, discovery, notification defaults, ACET, memory/MCP,
ingress control sources, device enrollment/renewal, huddle control calls,
meeting-audio upload defaults and CA fetch paths. The
consume timeout uses a private client value copy sharing this transport rather
than swapping the client's shared pointer. Caller timeout/redirect policy is
preserved. Explicit non-standard RoundTrippers retain caller custody; local
inference, peer/mesh and unrelated download transports are not rewritten.

WebSocket handshakes already make a fresh TCP dial. The resident Redis API
socket additionally expires after ten minutes, through the existing disconnect
and reconnect funnel. This retains backoff, rate-limit handling, subscription
restoration and the existing HTTP publish fallback. Its age timer is stopped
on disconnect and close; shutdown fences prevent a completed handshake from
publishing a connection after the client has closed.

Tests use owned httptest endpoints and an injected dialer that maps a stable
hostname to an old or replacement server. HTTP/1.1 and TLS HTTP/2 cases prove
the failure window and healthy maximum-age recovery, including an active old
HTTP/1.1 or HTTP/2 stream that finishes unchanged. Other fixtures cover privacy,
bounded body observation, binary replies, rate-limit hints, shutdown/subscriptions,
stale observations, query-insensitive distinct endpoints, sixty-second
cooldown boundaries, one-path outage storms and the long-poll client-copy
invariant. They do not contact live nodes, resolve a production hostname or
inspect node configuration.

If a pool has both stale and healthy sockets, successful responses on the
healthy socket can reset the streak. The independent ten-minute maximum age
still bounds reuse of the stale pool in that case.

Source CI is distinct from fleet acceptance. Running-node cutover acceptance
and release/deployment remain with the operators; this change does not perform
them. The separate #1476 self-heal policy is outside this transport slice.
