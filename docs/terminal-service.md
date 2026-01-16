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
# Start the terminal server (requires org-id)
citadel terminal-server --org-id my-org-id

# Start on a custom port
citadel terminal-server --org-id my-org-id --port 8080

# Start with custom idle timeout (in minutes)
citadel terminal-server --org-id my-org-id --idle-timeout 60
```

## Configuration

### Command-Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--org-id` | Organization ID for token validation (required) | - |
| `--port` | WebSocket server port | 7860 |
| `--idle-timeout` | Session idle timeout in minutes | 30 |
| `--shell` | Shell to use for sessions | Platform-specific |
| `--max-connections` | Maximum concurrent sessions | 10 |

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CITADEL_TERMINAL_PORT` | WebSocket server port | 7860 |
| `CITADEL_TERMINAL_ENABLED` | Enable/disable terminal service | true |
| `CITADEL_TERMINAL_IDLE_TIMEOUT` | Idle timeout in minutes | 30 |
| `CITADEL_TERMINAL_MAX_CONNECTIONS` | Max concurrent sessions | 10 |
| `CITADEL_TERMINAL_SHELL` | Shell to spawn | Platform default |
| `CITADEL_AUTH_HOST` | Authentication service URL | https://aceteam.ai |

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

## Authentication

### Token Validation Flow

1. Client connects to `/terminal?token=<token>`
2. Server validates token via `GET /api/fabric/terminal/tokens/{orgId}` on the auth service
3. Auth service returns token info (user ID, permissions, expiration)
4. On success: PTY session is created
5. On failure: Connection is closed with error message

### Implementing Token Validation

Your auth service must implement:

**Endpoint:** `GET ${CITADEL_AUTH_HOST}/api/fabric/terminal/tokens/{orgId}`

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
