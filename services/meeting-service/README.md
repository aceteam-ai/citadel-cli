# Meeting media stack (Citadel module)

Packages the meeting notetaker's media layer as an installable citadel module
(aceteam-ai/citadel-cli#514). One long-lived Linux container bundles **Chromium +
Xvfb + PulseAudio null sink + ffmpeg + a session supervisor (`meetingd`)**. Audio
is captured entirely in-container, so a node needs **no host Chromium, PulseAudio,
Xvfb, or ffmpeg** to run a meeting bot -- and the same image works on Linux and
inside Docker Desktop on macOS/Windows (the audio path is virtual, so there is no
host-audio dependency to break).

This is the first PR of the meeting-as-module effort: the installable-module
mechanism only. The existing `MEETING_JOIN` job handler is **unchanged**; a later
PR wires it to drive this container over loopback CDP.

## Why a health gate

The module's whole point is a **real health check**. `meetingd`'s `/health` runs a
canary: it loads a null sink, plays a generated 440 Hz tone into it, records the
sink monitor, and asserts the recording is **not silent**. It returns `200` only
when audio capture actually works, and `503` otherwise (5xx, so
`catalog.ProbeHealth` classifies it as unhealthy -- a `4xx` would be read as
healthy). Capability then means "this node can actually record a meeting", not
merely "a process is up". The live meeting crash that motivated this work recorded
**nothing** because there was no such gate.

## Architecture

```
  MEETING_JOIN handler (host, unchanged)
        | loopback CDP (127.0.0.1:8208)  ── socat ──> chromium :9222 (loopback)
        v
  meetingd (127.0.0.1:8207)  --launch-->  Chromium on Xvfb :99
        |                                    | PULSE_SINK=citadel_meeting_<id>
        | POST /sessions/{id}/record         v
        `--ffmpeg -f pulse -i <sink>.monitor -ac 1 -ar 16000 --> /workspace/<id>.wav
                                                                    (same mount the
                                                                     transcribe sidecar reads)
```

- **meetingd** (FastAPI) supervises per-meeting sessions: `POST /sessions` loads a
  null sink and launches Chromium routed into it; `POST /sessions/{id}/record`
  starts ffmpeg on the sink monitor; `.../record/stop` SIGINTs it (valid WAV
  trailer, same semantics as the host `NullSinkRecorder`); `DELETE /sessions/{id}`
  tears down. A minimal TTL reaper bounds orphaned sessions.
- **Chromium** launches with the host builder's exact flag set, including the two
  load-bearing flags `--autoplay-policy=no-user-gesture-required` (#5098: else the
  bot records silence) and `--password-store=basic` (#5122: build-independent
  cookie crypto for the seeded profile). One container delta: `--no-sandbox` (no
  setuid sandbox in the hardened container).
- **CDP over socat.** Modern Chromium binds its DevTools socket to `127.0.0.1`
  only and refuses a non-loopback bind, which a docker port publish can't reach.
  So Chromium listens on loopback `:9222` and a socat forwarder bridges the
  container-external `:9223` to it -- keeping the chrome flags identical to the
  host while still publishing the port. Both published ports bind `127.0.0.1` only
  (the sole consumer is the co-located citadel process).

## Ports

Registered in `services/ports.go` (the host-port registry) so no other module can
claim them:

| Purpose | Host port | Container port |
|---------|-----------|----------------|
| meetingd control API | 8207 | 8102 |
| Chromium CDP | 8208 | 9223 (socat → 9222) |

(The design brief named 8102/9223 for the host side; 8102 is already
`TEIEmbeddingPort`, so the **host** publish moved into the 8200 module block while
the container-internal ports stay 8102/9223.)

## Design note: PulseAudio (not pipewire-pulse)

The design brief named "pipewire-pulse"; this image uses plain **PulseAudio**. The
two are interchangeable for everything this stack depends on (the pulse socket,
`pactl`, `PULSE_SINK` routing, `ffmpeg -f pulse -i <sink>.monitor`), and the host
meeting code already treats them as equivalent. PulseAudio is the proven, simplest
headless null-sink capture path for the #1 historical risk here (silent audio), so
it is the lower-risk choice for the vertical slice.

## Profile (signed-in Google session)

`~/.citadel-cli/meeting-profile` is bind-mounted to `/profile` as a **host dir**
(never a compose-managed named volume) so the human-seeded, signed-in profile
survives container restart, module upgrade, and uninstall/reinstall. The
entrypoint clears stale Chromium singleton locks on boot (a lock left by a
previous container has a different hostname, which otherwise bricks Chromium).

Constraint: Chromium refuses a profile written by a **newer** Chromium, so a
host-seeded profile must be seeded with a Chromium no newer than this image's. The
durable fix (in-container seeding) is a follow-up.

## Build the image

```sh
docker build -t ghcr.io/aceteam-ai/meeting-service:latest services/meeting-service
```

CI publishes it on pushes to `main` that touch this dir
(`.github/workflows/build-meeting-service.yml`). v1 is **amd64-only**; the
`service.yaml` declares `arch: [amd64]` to match. arm64 is a follow-up.

## Install as a module

Publish `service.yaml` + `compose.yml` from this dir as
`services/meeting/{service.yaml,compose.yml}` in the catalog repo
(`aceteam-ai/citadel-services`), then on a node:

```sh
citadel module install meeting
```

## Tests

```sh
# Go: manifest schema + port registry (runs in CI via `go test ./...`)
go test ./services/...

# Python unit tests (chrome flags, ffmpeg format, path safety, RMS, health gate)
pip install -r requirements.txt pytest httpx
python3 -m pytest services/meeting-service/test_meetingd.py

# End-to-end acceptance (needs docker + websocket-client): builds the image,
# proves canary-healthy + a non-silent WAV driven through Chromium CDP on a box
# with no host chrome/pulse/xvfb.
pip install websocket-client
python3 services/meeting-service/smoke_test.py
```
