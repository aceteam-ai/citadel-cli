# Citadel Terminal Service

The Citadel Terminal Service provides WebSocket-based terminal access to nodes running the Citadel agent. This enables browser-based terminal sessions through the AceTeam web application.

## Overview

The terminal service creates a WebSocket server that:

1. Authenticates incoming connections using tokens validated against the AceTeam API
2. Spawns PTY (pseudo-terminal) sessions for authenticated users
3. Streams terminal input/output bidirectionally over WebSocket
4. Manages session lifecycle, idle timeouts, and resource limits

## Quick Start

```bash
# Start using org-id from manifest (set during 'citadel init')
citadel terminal-server

# Start with explicit organization ID
citadel terminal-server --org-id my-org-id

# Start on a custom port
citadel terminal-server --port 8080

# Start with custom idle timeout (in minutes)
citadel terminal-server --idle-timeout 60

# Start in test mode (accepts any token, for development)
citadel terminal-server --test

# Integrated with citadel work command
citadel work --mode=nexus --terminal --terminal-port 7860
```

## Configuration

### Command-Line Flags

**Standalone Terminal Server (`citadel terminal-server`):**

| Flag | Description | Default |
|------|-------------|---------|
| `--org-id` | Organization ID for token validation | From manifest |
| `--port` | WebSocket server port | 7860 |
| `--idle-timeout` | Session idle timeout in minutes | 30 |
| `--shell` | Shell to use for sessions | Platform-specific |
| `--max-connections` | Maximum concurrent sessions | 10 |
| `--test` | Test mode - accepts any token (for development) | false |

**Integrated with Work Command (`citadel work`):**

| Flag | Description | Default |
|------|-------------|---------|
| `--terminal` | Enable terminal WebSocket server | false |
| `--terminal-port` | Terminal server port | 7860 |

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CITADEL_TERMINAL_PORT` | WebSocket server port | 7860 |
| `CITADEL_TERMINAL_ENABLED` | Enable/disable terminal service | true |
| `CITADEL_TERMINAL_IDLE_TIMEOUT` | Idle timeout in minutes | 30 |
| `CITADEL_TERMINAL_MAX_CONNECTIONS` | Max concurrent sessions | 10 |
| `CITADEL_TERMINAL_SHELL` | Shell to spawn | Platform default |
| `CITADEL_TERMINAL_SESSION` | Persistent tmux session base name to back connections, or a disable sentinel (`none`/`off`/`disabled`/`false`/`0`, case-insensitive) to force a bare shell | `citadel` (tmux backing ON) |
| `CITADEL_TERMINAL_SESSION_TTL` | Retention lease after disconnect for a Citadel-managed tmux session; minimum non-zero value `1m`, Go duration syntax, `0` disables the reaper | `168h` (7 days) |
| `CITADEL_TMUX_BIN` | Explicit path to a tmux binary (overrides PATH/managed lookup) | (unset) |
| `CITADEL_AUTH_HOST` | Authentication service URL | https://aceteam.ai |
| `CITADEL_TOKEN_REFRESH_INTERVAL` | Token cache refresh interval in minutes | 60 |

### Persistent tmux Sessions

By default (`CITADEL_TERMINAL_SESSION` unset, base name `citadel`), and as long
as a usable `tmux` binary is available, each WebSocket connection is backed by a
named tmux session instead of a fresh bare
shell. The tmux server keeps the session alive after a client disconnects, so
reconnecting re-attaches to the same session and the terminal state (running
programs, scrollback, working directory) survives reconnects. This backs the
"chat to a node" path in the mobile/desktop apps, and is what makes a dropped
`citadel connect`/web console session resumable.

tmux is never assumed to be installed. The binary is resolved in order:
`CITADEL_TMUX_BIN` → `tmux` on `PATH` → a Citadel-managed binary at
`~/.citadel/bin/tmux`. When none is found, the server falls back to a bare shell
(connections still work, but do not persist across reconnects).

**Opting out (citadel #780):** a power user who runs their own tmux — connecting
through the Citadel console, sshing elsewhere, running `tmux a` there — ends up
nested inside Citadel's outer tmux session: stacked status bars, prefix-key
collisions. Set `CITADEL_TERMINAL_SESSION` to a disable sentinel (`none`, `off`,
`disabled`, `false`, or `0`, case-insensitive, matched after trimming
whitespace) to force every connection on that node back to a fresh, non-
persistent bare shell — no tmux wrap at all. This is a node-wide setting (it
governs any connection that does not itself request a session, i.e. the web
console and any token-based client); the CLI's `citadel ssh`/`citadel connect`
already default to a bare shell independently of this setting and only opt into
persistence with `--tmux` (citadel #759), so a plain `citadel connect` never
nests regardless of the node's own default.

The server also falls back to a bare shell — without even trying to resolve a
tmux binary — when the citadel process itself is already running inside a
tmux client (`$TMUX` is set in its own environment, e.g. an operator manually
running `citadel work` from inside their own tmux window). Wrapping a new
connection in `tmux new-session` on top of that would nest a tmux client
inside another one on the same node, breaking prefix keys and stacking status
bars (citadel #751). This nesting avoidance applies ONLY to the AUTO path —
`CITADEL_TERMINAL_SESSION` (the node's own default) with no explicit request
from the connecting caller. An explicit `citadel ssh`/`citadel connect --tmux`
request is a deliberate, informed ask and is honored even inside tmux; only
the auto-started default backs off. The terminal server logs a distinct
"already inside a tmux session" note when this happens, so it's diagnosable
in server logs rather than looking like a missing-binary problem.

Starting a session is decoupled from launching `claude`: the session is just a
shell. Launching an agent inside it is a separate, explicit step (e.g. sending
keys to the session once `claude` is installed).

Sessions can also be pre-created, listed, or checked out-of-band through the
`TMUX_SESSION` job type (payload `action`: `ensure`|`create`|`list`|`has`,
`name`, optional `shell`), dispatched through the standard worker mechanism.

Citadel marks only sessions it successfully creates; it never adopts an
existing unmarked session. If the derived name collides with an operator-owned
session, the connection logs the collision and safely falls back to a bare
shell without attaching to or modifying that session.

Citadel gives each marked session an explicit retention lease and sweeps once
per hour. The lease is renewed while a client is attached and once when it
disconnects. A session becomes eligible only after the lease expires and it is
detached. This is deliberately not called an idle timeout: tmux's
`session_activity` does not reflect detached pane output or `send-keys`, so it
cannot prove that a background task is idle. Consequently, a still-running task
in a disconnected session may be terminated after the configured retention
lease. At removal, tmux atomically checks that the session is still detached,
still marked, and still carries the exact expired lease that was observed; an
attach or renewal racing the sweep therefore fails closed. Unmarked and
malformed sessions are never removed. Set the TTL to `0` to disable cleanup.

### Surviving a managed worker restart

On Linux, a tmux server started by `citadel.service` would normally inherit the
service control group. systemd's default stop behavior kills the whole control
group, including the tmux server, when Citadel restarts. Citadel avoids that by
launching tmux create and attach commands in a separate transient scope when it
is running as a systemd user service. A root system service uses an equivalent
system scope. Other worker subprocesses remain in the Citadel control group and
retain normal cleanup behavior.

The scope names begin with `citadel-session-`. They include the session name,
worker PID, and a sequence number so concurrent connections cannot collide.
The scope owns the tmux server or attached client, not the WebSocket itself.
Closing a WebSocket still detaches normally, and reconnecting reaches the same
tmux session.

Non-systemd platforms and interactive `citadel work` invocations keep invoking
tmux directly. A Linux system service running as a non-root user also keeps the
direct behavior because that user cannot safely create a sibling system scope.
Each launch writes one diagnostic line to the Citadel log stating whether it
used a transient scope or the direct fallback, including the fallback reason.

Sessions created by a pre-upgrade binary remain unscoped and therefore still
die on the first worker restart after upgrading; sessions created or reattached
after the upgrade use the new scope behavior.

Upgrade transition: a pre-existing unmarked session that collides with a
Citadel name is no longer adopted; rename or remove it explicitly before
reconnecting if it should become Citadel-managed.

#### Manual restart proof

Run this only with a dedicated test account or test node. The procedure uses a
private tmux socket, so it never lists, attaches to, or stops the account's
ordinary tmux sessions.

1. Create a temporary `tmux` wrapper that selects a private server socket:

   ```bash
   proof_dir=$(mktemp -d /tmp/citadel-1166.XXXXXX)
   real_tmux=$(command -v tmux)
   printf '#!/bin/sh\nexec %s -L citadel-1166 "$@"\n' "$real_tmux" >"$proof_dir/tmux"
   chmod 700 "$proof_dir/tmux"
   test ! -e "${TMUX_TMPDIR:-/tmp}/tmux-$(id -u)/citadel-1166"
   ```

   The final `test` must succeed. If it does not, choose a different private
   socket name before continuing.

2. Point the test account's systemd user manager at the wrapper, then restart
   the test worker once so it inherits the setting:

   ```bash
   systemctl --user set-environment CITADEL_TMUX_BIN="$proof_dir/tmux"
   systemctl --user restart citadel.service
   ```

3. Dispatch this job to the test node through the normal job path:

   ```json
   {"type":"TMUX_SESSION","payload":{"action":"ensure","name":"citadel1166proof"}}
   ```

4. Confirm the private session and its separate scope exist:

   ```bash
   tmux -L citadel-1166 has-session -t citadel1166proof
   systemctl --user list-units 'citadel-session-citadel1166proof-*.scope'
   ```

5. Restart Citadel and confirm the same private session is still present:

   ```bash
   systemctl --user restart citadel.service
   tmux -L citadel-1166 has-session -t citadel1166proof
   ```

6. Clean up only the isolated proof session and restore the worker environment:

   ```bash
   tmux -L citadel-1166 kill-session -t citadel1166proof
   systemctl --user unset-environment CITADEL_TMUX_BIN
   systemctl --user restart citadel.service
   rm -r "$proof_dir"
   ```

The final `has-session` command must exit zero. Before this fix it reports no
server after the service restart because the tmux server remains inside
`citadel.service` and is killed with the worker.

### Platform Defaults

**Shell Selection:**
- Linux: `$SHELL` environment variable, or `/bin/bash`
- macOS: `/bin/zsh`
- Windows: Not supported (PTY requires ConPTY implementation)

## Architecture

```
┌─────────────────┐     WebSocket      ┌─────────────────────┐
│  Browser/Client │ ◄────────────────► │  Terminal Server    │
│                 │                    │                     │
│  - Connect      │                    │  - Auth validation  │
│  - Send input   │                    │  - Rate limiting    │
│  - Recv output  │                    │  - Session mgmt     │
│  - Resize       │                    │  - PTY management   │
└─────────────────┘                    └─────────┬───────────┘
                                                 │
                                                 │ PTY
                                                 ▼
                                       ┌─────────────────────┐
                                       │  Shell Process      │
                                       │  (bash/zsh/etc)     │
                                       └─────────────────────┘
```

## Integration with Work Command

The terminal server can be started as part of `citadel work`, running alongside job processing:

```bash
# Start worker with terminal server enabled
citadel work --mode=nexus --terminal

# With custom terminal port
citadel work --mode=nexus --terminal --terminal-port 8080

# Combined with other work features
citadel work --mode=nexus --terminal --heartbeat --ssh-sync
```

**Architecture (Integrated Mode):**

```
┌──────────────────────────────────────────────────────────────┐
│                    citadel work --terminal                    │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌────────────────┐  ┌────────────────┐  ┌───────────────┐  │
│  │  Job Worker    │  │  Terminal      │  │  Heartbeat    │  │
│  │  (Nexus/Redis) │  │  Server        │  │  Publisher    │  │
│  └────────────────┘  └────────────────┘  └───────────────┘  │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

**Notes:**
- Uses org-id from manifest (must run `citadel init` first)
- Token validation uses the same auth service as other work features
- Terminal server runs in a goroutine alongside the main worker loop

## Authentication

### Token Caching (CachingTokenValidator)

In production mode, the terminal server uses a **caching token validator** to avoid API calls on every connection:

```
┌─────────────────┐                    ┌─────────────────────┐
│  Client         │                    │  Terminal Server    │
│  (with token)   │ ──────────────────►│                     │
└─────────────────┘                    │  1. Hash token      │
                                       │  2. Check cache     │
                                       │  3. If found: allow │
                                       └─────────┬───────────┘
                                                 │
                  Background refresh (hourly)    │
                                                 ▼
                                       ┌─────────────────────┐
                                       │  AceTeam API        │
                                       │  (token list)       │
                                       └─────────────────────┘
```

**How It Works:**

1. On startup, the server fetches all valid token **hashes** from the API
2. Incoming tokens are hashed with SHA-256 and compared locally (no API call)
3. The cache is refreshed hourly (configurable via `CITADEL_TOKEN_REFRESH_INTERVAL`)
4. On cache miss, an immediate refresh is triggered before rejecting
5. Exponential backoff (1s → 5min) is used on API failures

**Benefits:**
- Fast validation (no network round-trip per connection)
- Reduced load on the auth service
- Continues working during brief API outages

### Token Validation Flow

1. Client connects to `/terminal?token=<token>`
2. Server hashes token with SHA-256 and checks local cache
3. If cache miss: refreshes from API and checks again
4. On cache hit: validates org ID and expiration
5. On success: PTY session is created
6. On failure: Connection is closed with error message

### Test Mode

For development and testing, use the `--test` flag:

```bash
citadel terminal-server --test
```

In test mode:
- Accepts `test-token` as a valid token
- No auth service connection required
- **Not for production use**

### Implementing Token Validation (API Requirements)

Your auth service must implement two endpoints:

**1. Token List Endpoint (for caching):**

`GET ${CITADEL_AUTH_HOST}/api/fabric/terminal/tokens/{orgId}`

Returns SHA-256 hashes of all valid tokens for the organization.

**Response (200 OK):**
```json
{
  "tokens": [
    {
      "hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      "user_id": "user-123",
      "org_id": "org-456",
      "expires_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

**2. Single Token Validation (fallback):**

`GET ${CITADEL_AUTH_HOST}/api/fabric/terminal/tokens/{orgId}`

**Headers:**
- `Authorization: Bearer <token>`

**Success Response (200 OK):**
```json
{
  "user_id": "user-123",
  "org_id": "org-456",
  "node_id": "node-789",
  "expires_at": "2024-01-01T00:00:00Z",
  "permissions": ["terminal:connect"]
}
```

**Error Responses:**
- `401 Unauthorized`: Invalid or expired token
- `403 Forbidden`: User not authorized for this organization
- `503 Service Unavailable`: Auth service temporarily unavailable

## WebSocket Protocol

### Connection URL

```
ws://host:port/terminal?token=<auth-token>
```

### Message Format

All messages are JSON-encoded:

```json
{
  "type": "input|output|resize|error|ping|pong",
  "payload": "<base64-encoded data>",
  "cols": 80,
  "rows": 24,
  "error": "error message"
}
```

### Message Types

| Type | Direction | Description |
|------|-----------|-------------|
| `input` | Client → Server | Terminal input data |
| `output` | Server → Client | Terminal output data |
| `resize` | Client → Server | Resize terminal (cols, rows) |
| `error` | Server → Client | Error message |
| `ping` | Either | Connection health check |
| `pong` | Either | Response to ping |

### Example Messages

**Input (client sends keystrokes):**
```json
{"type": "input", "payload": "bHM="}
```

**Output (server sends terminal output):**
```json
{"type": "output", "payload": "dG90YWwgMTIK..."}
```

**Resize (client resizes terminal):**
```json
{"type": "resize", "cols": 120, "rows": 40}
```

**Error (server reports error):**
```json
{"type": "error", "error": "session closed due to idle timeout"}
```

## Security Considerations

### Rate Limiting

The server implements per-IP rate limiting for connection attempts:
- Default: 1 request per second with burst of 5
- Prevents brute-force token guessing attacks

### Connection Limits

- Maximum concurrent connections are enforced
- Default limit: 10 concurrent sessions
- Prevents resource exhaustion attacks

### Session Isolation

- Each WebSocket connection gets its own PTY session
- Sessions run as the user who started the terminal server
- Sessions are isolated from each other

### Idle Timeout

- Sessions are automatically closed after inactivity
- Default timeout: 30 minutes
- Prevents resource leaks from abandoned sessions

### Origin Validation

The server validates WebSocket origins:
- Allows `localhost` and `127.0.0.1` for development
- Allows `aceteam.ai` domains
- Blocks connections from unknown origins

## Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/terminal` | WebSocket | Terminal session endpoint |
| `/health` | GET | Health check (returns session count) |

### Health Check Response

```json
{"status": "ok", "sessions": 3}
```

## Platform Support

| Platform | PTY Support | Status |
|----------|-------------|--------|
| Linux | `creack/pty` | Supported |
| macOS | `creack/pty` | Supported |
| Windows | ConPTY | Not yet supported |

**Note:** Windows support requires ConPTY implementation, which is planned for a future release.

## Troubleshooting

### Common Errors

**"PTY terminal sessions are not yet supported on Windows"**
- The terminal server uses Unix PTY which isn't available on Windows
- Run the terminal server on a Linux or macOS host

**"invalid or expired authentication token"**
- The token was rejected by the auth service
- Ensure the token is valid and not expired
- Check that the org-id matches the token's organization

**"rate limit exceeded"**
- Too many connection attempts from the same IP
- Wait before retrying

**"maximum connections reached"**
- The server has reached its connection limit
- Close unused sessions or increase `--max-connections`

### Debug Mode

For troubleshooting, check the server logs for connection events and errors.
