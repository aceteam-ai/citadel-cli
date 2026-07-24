# Capability Toggles

Citadel node capabilities are **config toggles**, but they come in two postures
depending on risk:

- **Default-ON (opt-out)** — low-stakes capabilities (meeting notetaker,
  telemetry, keep-awake, services/ssh/provision). A node that *can* do the thing
  advertises it and registers the handler out of the box; the operator may **opt
  out** from the Control Center, and the platform may set the same value
  programmatically via `APPLY_DEVICE_CONFIG`.
- **Default-OFF (opt-in) + passcode** — sensitive remote-access surfaces
  (**console/terminal**, **screen/VNC/desktop**, **file hosting/browsing**, and
  **shell**). A freshly joined node does **not** expose these: it neither
  advertises them nor serves the corresponding jobs until the operator explicitly
  enables each one, and even then a **per-node passcode** gates actual access.

The default-ON pattern mirrors the opt-out model used for anonymous telemetry
(`internal/config/telemetry.go`) and the meeting capability
(`internal/config/meeting.go`). The rest of this doc first describes that pattern,
then the sensitive default-OFF + passcode posture (aceteam#6524) and **why they
differ**.

## The default-ON (opt-out) pattern

Each capability toggle is a small per-concern config file in
`platform.ConfigDir()` with a default-true value, following the
Telemetry/Meeting shape:

- **Config** (`internal/config/<cap>.go`): a struct with one bool, a
  `Default<Cap>()` returning **enabled**, and `Load<Cap>` / `Save<Cap>`. Absent
  file or absent key falls back to the default (enabled).
- **Detector** (`internal/capabilities/detector.go`): the capability is
  advertised only when `Load<Cap>(...).<Enabled> && depsPresent`. The toggle
  gates advertisement; the deps gate feasibility. Both must hold.
- **Handler** (`internal/worker/handler_adapter.go`): the job handler registers
  under the same `Load<Cap>(...).<Enabled>` gate, so a node that does not
  advertise the tag also refuses the job.
- **Control Center** (`internal/tui/controlcenter/settings_page.go`): a numbered
  toggle in the Settings pane that `Save<Cap>`s the flip. Applies on the next
  worker start / capability detection.
- **Programmatic** (`internal/jobs/config_handler.go`): `DeviceConfig` carries a
  `*bool` field (pointer so an omitted field is a no-op, not a silent opt-out).
  When non-nil, `APPLY_DEVICE_CONFIG` writes the **same** per-concern config file
  the detector and Control Center use.

### Precedence

There is a **single persisted toggle** per capability. Both the Control Center
and `APPLY_DEVICE_CONFIG` write that one file, so the effective value is
**last-writer-wins**, defaulting to **enabled** when neither has written it. (The
org owns the node config, so a device-config push re-applying an opt-out over a
local change is intentional — same as VNC/SSH.)

### Concrete instance: the `meeting` capability (aceteam#5098)

The auto-join meeting notetaker is the first capability wired this way:

- Config: `internal/config/meeting.go` — key `meeting_enabled` in `meeting.yaml`,
  default **true**.
- Advertised as the `meeting` tag when `meeting_enabled` is set **and** the audio
  + Chromium + Xvfb deps are present.
- Control Center: Settings pane, toggle **5** ("Meeting Notetaker").
- Programmatic: `DeviceConfig.MeetingEnabled *bool` (`meetingEnabled` in the
  APPLY_DEVICE_CONFIG payload), driven by the platform's `citadel_config` MCP
  tool.

## The default-OFF (opt-in) + passcode posture (aceteam#6524)

The sensitive remote-access surfaces live in a single per-node config,
`internal/config/permissions.go` (`permissions.yaml`), which the HTTPS gateway
and the mesh listeners read:

| Permission  | Surface                                  | Default |
|-------------|------------------------------------------|---------|
| `console`   | Terminal/shell WebSocket access          | **OFF** |
| `desktop`   | Screen/VNC, screenshots, remote actions  | **OFF** |
| `files`     | Filesystem browse/host (FILE_* jobs)     | **OFF** |
| `shell`     | `SHELL_COMMAND` (arbitrary code as root)  | **OFF** |
| `services`  | Service list/management                  | ON      |
| `ssh`       | SSH authorized_keys sync                 | ON      |
| `provision` | Container provisioning API               | ON      |

`DefaultPermissions()` returns the console/desktop/files/shell surfaces
**disabled**. This flips the previous default-on model.

### Why these three differ from meeting/telemetry

Telemetry is anonymous debug data; the meeting notetaker joins a call the org
already scheduled. Neither exposes the operator's own machine. Console, desktop,
and files are categorically different: the moment a node joins the fabric,
default-on put the operator's **terminal, screen, and filesystem** on the org
mesh before they decided to expose them. For a privacy-first operator ("your data
stays in this box") that is a trust-breaking surprise (the White Whale onboarding
incident). So these surfaces are **default-deny, opt-in** — joining to serve a
model must never, by itself, expose remote access. Inference/model-serving is
**not** a permission and is unaffected: a node serves models with all three
surfaces off.

### The passcode gate — "enabled" is not "open"

Enabling a surface is not the same as opening it to anyone on the org mesh. A
**per-node passcode** (bcrypt hash in `permissions.yaml`, never plaintext) gates
actual access:

- Set/verified via `Permissions.SetPasscode` / `Permissions.VerifyPasscode`
  (bcrypt, salt embedded in the hash).
- **Scope:** one passcode **per node**, with **per-capability enablement**
  (enable console and desktop independently; one passcode unlocks whatever is
  enabled). This matches the issue's recommended scope.
- **Fail-closed:** a surface that is enabled but has **no passcode set** stays
  denied. Enablement without a passcode never silently opens the surface.

### Enforcement points (all must hold; each fails closed)

1. **Advertisement** (`internal/status/capabilities_flags.go`): the heartbeat
   capability flags for console/desktop/files report available only when the
   node is capable **and** the permission is enabled. A fresh node reports them
   false, so the web console does not present a live surface. (Enablement stays
   separately visible via the `PermissionState` heartbeat block, so the web can
   still render an "enable + set passcode" call to action.)
2. **Redis fabric handlers** (`internal/worker/handler_adapter.go`): the
   VNC/screenshot/actions handlers and the FILE_* browse/host handlers are
   registered **only** when `desktop` / `files` are enabled (`DesktopDisabled` /
   `FilesDisabled` opts). A fresh node refuses those jobs. `TRANSCRIBE_AUDIO` and
   `MEETING_JOIN` share the workspace but belong to the default-ON meeting
   capability and are deliberately **not** gated.
   - **Shell** (`SHELL_COMMAND`) is the odd one out: it has **no gateway/HTTP
     route** (`categoryForPath` never returns `shell`), so it is gated entirely on
     the Redis job path. The handler is always registered (so a disabled node
     returns the "disabled" refusal rather than "unsupported job type"), but it
     refuses unless `shell` is enabled **and** the job presents the correct node
     passcode. The passcode travels in the `SHELL_COMMAND` payload's `passcode`
     field (there is no header, unlike console/desktop/files). Fail-closed: an
     enabled handler whose passcode verifier was never wired, or a wrong/absent
     payload passcode, refuses every command. This governs the platform's
     node-management tabs (logs/services/docker), which run shell commands.
3. **HTTPS gateway** (`internal/gateway/gateway.go`): `permissionMiddleware`
   blocks a disabled category, and for an enabled sensitive category additionally
   requires the passcode (`X-Citadel-Passcode` header, or `?passcode=` for
   WebSocket clients).
4. **Direct mesh listeners** — because a mesh peer can dial these listeners
   directly (not only through the gateway):
   - **Terminal server** (`internal/terminal/`): started only when `console` is
     enabled (`cmd/work.go`), and every connection additionally requires the
     passcode (`Config.PasscodeVerifier`) on top of token/mesh-identity auth.
   - **Desktop endpoints** on the status server (`internal/status/server.go`
     `/api/screenshot`, `/api/actions`): gated on `desktop`, and `requireAuth`
     additionally requires the passcode (`ServerConfig.PasscodeVerifier`).

### Enabling a surface

- **Control Center:** the Built-in Services modal toggles
  console/desktop/files/shell. Enabling one with no passcode set warns that
  access stays denied until a passcode is set.
- **Programmatic (`APPLY_DEVICE_CONFIG`):** `DeviceConfig` carries
  `consoleEnabled` / `desktopEnabled` / `filesEnabled` / `shellEnabled` (`*bool`,
  nil = untouched) and `nodePasscode` (`*string`, bcrypt-hashed before persist;
  empty clears it). This writes the same `permissions.yaml` the gates read. To
  enable remote shell for the node-management tabs, send
  `{"shellEnabled": true, "nodePasscode": "<pin>"}`; the tabs must then present
  that same `<pin>` in each `SHELL_COMMAND` payload's `passcode` field.

### Cross-repo follow-up (aceteam web console, not in this repo)

The web console should render console/desktop/files/shell as **opt-in** (an
"enable + set passcode" call to action) rather than presenting a live surface for
a node that has not enabled it, and must send the passcode on the
`X-Citadel-Passcode` header (or `?passcode=` for the WebSocket terminal/VNC). For
**shell** specifically (the node-management logs/services/docker tabs, aceteam
#6559), the enable call is `APPLY_DEVICE_CONFIG {"shellEnabled": true,
"nodePasscode": "<pin>"}` and each dispatched `SHELL_COMMAND` must carry the pin
in its `passcode` payload field (shell has no HTTP header path). How the web
attaches that pin per dispatch (human-entered once vs stored for the session) is
the web side's decision. Track alongside the mesh-ACL / cross-org work
([[citadel-mesh-security]] #5028): that work is about cross-org exposure, this is
per-node owner consent.
