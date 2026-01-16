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
| `CITADEL_AUTH_HOST` | Authentication service URL | https://aceteam.ai |
| `CITADEL_TOKEN_REFRESH_INTERVAL` | Token cache refresh interval in minutes | 60 |

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
