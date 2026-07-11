# Capability Toggles

Node capabilities in Citadel are **config toggles, default opted-in**. A node
that has the dependencies for a capability advertises it (and registers the
matching job handler) out of the box; the operator may **opt out** from the
Control Center, and the platform may set the same value **programmatically** via
`APPLY_DEVICE_CONFIG`. This mirrors the opt-out model already used for anonymous
telemetry (`internal/config/telemetry.go`).

## The pattern

Each capability toggle is a small per-concern config file in
`platform.ConfigDir()` with a default-true value, following the
Telemetry/KeepAwake shape:

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

## Concrete instance: the `meeting` capability (aceteam#5098)

The auto-join meeting notetaker is the first capability wired this way:

- Config: `internal/config/meeting.go` — key `meeting_enabled` in `meeting.yaml`,
  default **true**.
- Advertised as the `meeting` tag when `meeting_enabled` is set **and** the audio
  + Chromium + Xvfb deps are present.
- Control Center: Settings pane, toggle **5** ("Meeting Notetaker").
- Programmatic: `DeviceConfig.MeetingEnabled *bool` (`meetingEnabled` in the
  APPLY_DEVICE_CONFIG payload), driven by the platform's `citadel_config` MCP
  tool.
