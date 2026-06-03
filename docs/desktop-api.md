# Desktop API Reference

Citadel exposes REST endpoints for desktop automation when a display is available and auth is configured. These endpoints power Agent Workspaces — AI agents that operate GUIs on Citadel nodes.

## Prerequisites

**Linux:**
```bash
sudo apt-get install xdotool imagemagick
# or: sudo apt-get install xdotool scrot
```

**macOS/Windows:** Not yet implemented. Stubs exist with TODO notes for platform-specific tools (`screencapture`/`cliclick` on macOS, PowerShell on Windows).

## Authentication

All desktop endpoints require a bearer token. The same token used for terminal access works here.

```
Authorization: Bearer <token>
```

Tokens are validated against the AceTeam API via `CachingTokenValidator`, which fetches and caches token hashes with periodic refresh.

## Endpoints

### GET /api/screenshot

Captures a PNG screenshot of the current display.

**Response:** `image/png` body (raw PNG bytes)

**Example:**
```bash
curl -s -H "Authorization: Bearer $TOKEN" http://node-ip:8080/api/screenshot > screenshot.png
```

**How it works:** Tries `import -window root png:-` (ImageMagick) first, falls back to `scrot -o -`. Both write PNG to stdout. If neither tool is available, returns 500 with an error message.

### POST /api/actions

Executes a sequence of mouse/keyboard actions on the display.

**Request body:** JSON array of action objects.

**Response:**
```json
{"ok": true, "actions": 3}
```

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '[
    {"type": "move", "x": 500, "y": 300},
    {"type": "click", "x": 500, "y": 300, "button": 1},
    {"type": "type", "text": "hello world"}
  ]' \
  http://node-ip:8080/api/actions
```

### Action Types

| Type | Required Fields | Description |
|------|----------------|-------------|
| `move` | `x`, `y` | Move cursor to coordinates |
| `click` | `x`, `y` | Click at coordinates. Optional `button` (1=left, 2=middle, 3=right, default 1) |
| `type` | `text` | Type a text string (max 1000 chars) |
| `key` | `key` | Press a key combination (e.g. `Return`, `ctrl+c`, `alt+F4`) |
| `scroll` | `delta` | Scroll. Positive = up, negative = down (range -100 to 100) |

### Validation

All actions are strictly validated before execution:

- **Action type allowlist:** Only `move`, `click`, `type`, `key`, `scroll` are accepted
- **Coordinate bounds:** 0-32767 for x and y
- **Button range:** 1-5
- **Text sanitization:** Control characters rejected
- **Key name pattern:** Only alphanumeric, modifiers, and common key names
- **Max actions per request:** 100
- **Max text length:** 1000 characters

Invalid actions return 400 with a descriptive error indicating which action failed validation.

### Implementation

On Linux, actions are translated to `xdotool` commands:

| Action | xdotool Command |
|--------|----------------|
| `move` | `xdotool mousemove -- X Y` |
| `click` | `xdotool mousemove -- X Y click BUTTON` |
| `type` | `xdotool type --clearmodifiers -- TEXT` |
| `key` | `xdotool key --clearmodifiers KEY` |
| `scroll` | `xdotool click 4` (up) or `click 5` (down), repeated N times |

All commands use `exec.Command` argument slices (never shell invocation) to prevent injection.

## Capability Detection

The `/status` endpoint includes desktop capabilities:

```json
{
  "desktop": {
    "os": "linux",
    "os_version": "Ubuntu 24.04.1 LTS",
    "display": ":0",
    "screen_resolution": "1920x1080",
    "vnc_port": 5900
  }
}
```

Detection methods:
- **OS version:** Parsed from `/etc/os-release` (`PRETTY_NAME`)
- **Display:** `$DISPLAY` or `$WAYLAND_DISPLAY` environment variables
- **Resolution:** `xrandr --query` output parsing
- **VNC port:** Scans ports 5900-5901 via `ss -tlnp`
