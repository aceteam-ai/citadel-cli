# macOS VNC Provisioner — On-Device E2E Runbook

This runbook covers the manual, on-device verification of the macOS VNC
provisioner (`DarwinVNCManager` in `internal/platform/vnc.go`). It exists because
the activate/configure/start path **cannot run in CI**: it requires `sudo` and
enables Screen Sharing system-wide. Unit tests only cover command construction
(`kickstart*Args`) and detection (`citadel vnc status` with Screen Sharing off).

These steps must be run by an operator on a real Mac. Tracking issue: **#268**
(follow-up to #267, parent #122).

## What the provisioner does

macOS ships a VNC server as part of Remote Management / Screen Sharing, so there
is nothing to install. The provisioner drives Apple's `kickstart` tool:

```
/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart
```

- **enable** → `kickstart -activate -configure -access -on -clientopts -setvnclegacy -vnclegacy yes -clientopts -setvncpw -vncpw <pw> -restart -agent -privs -all`
- **disable** → `kickstart -deactivate -configure -access -off`
- **status** → `launchctl list com.apple.screensharing` (with an `lsof` port-5900 fallback)

All `kickstart` invocations require root; when not root the provisioner returns
`ErrDarwinSudoRequired` (`sudo citadel vnc enable`) rather than a wrapped error.

## Prerequisites

- A macOS node with **Screen Sharing OFF** (System Settings → General → Sharing).
- An admin account able to run `sudo`.
- The latest `citadel` binary deployed to the node (`darwin/arm64` or
  `darwin/amd64` as appropriate).
- A second machine on the same network (or Headscale mesh) with a VNC / noVNC
  client to connect back into the Mac.

## Procedure

### 1. Baseline — confirm starting state

```bash
citadel vnc status
```

Expect:

```
Installed: true        # kickstart tool is always present on macOS
Running:   false       # Screen Sharing is off
```

`Installed: true` is expected even before enabling — it only means the built-in
Remote Management tool exists. No `Port:` line is printed while not running.

### 2. Verify the no-sudo error path

Run **without** sudo first to confirm the error surfaces cleanly:

```bash
citadel vnc enable
```

Expect the process to fail with the actionable message (not a wrapped/opaque
error):

```
macOS Screen Sharing provisioning requires root privileges. Run: sudo citadel vnc enable
```

### 3. Enable with sudo

```bash
sudo citadel vnc enable
# optional explicit password (max 8 chars, VNC DES limit):
# sudo citadel vnc enable --password hunter2x
```

If no `--password` is given, an 8-character password is generated and printed to
stdout. **Record it** — it is needed to connect. Passwords longer than 8 chars
are truncated (you will see a truncation notice). macOS Screen Sharing only
listens on port **5900**; a non-default `--port` is accepted but ignored with a
note.

### 4. Confirm running status

```bash
citadel vnc status
```

Expect:

```
Installed: true
Running:   true
Port:      5900
```

`Running: true` comes from `launchctl list com.apple.screensharing` returning
exit 0 once Screen Sharing is loaded.

### 5. Confirm in System Settings

Open **System Settings → General → Sharing** and confirm **Remote Management**
(or **Screen Sharing**) is now enabled, and that a VNC password was set under its
options. This is the system-wide change that cannot be exercised in CI.

### 6. Connect end-to-end

From the second machine, connect to the Mac on port 5900 using the recorded
password, via either:

- a standard VNC client (e.g. `vnc://<mac-ip>:5900`), or
- the DesktopViewer / noVNC chain (the same flow verified on Windows in #119).

Confirm the macOS desktop **renders** and **accepts keyboard/mouse input**.

### 7. Disable and confirm teardown

```bash
sudo citadel vnc disable
citadel vnc status
```

Expect `disable` to run `kickstart -deactivate` and `status` to report:

```
Installed: true
Running:   false
```

Re-confirm in System Settings → Sharing that Remote Management is back **off**,
and that the previous VNC client can no longer connect.

There is nothing to uninstall on macOS (the server is a built-in component);
`citadel vnc disable --uninstall` maps to the same deactivate path as `disable`.

## Caveats to watch for (from #267)

- **TCC / Privacy prompts.** macOS may gate Screen Recording / Accessibility
  behind TCC. A headless fleet agent cannot click through these prompts. On a
  managed fleet, pre-grant the relevant TCC permissions (e.g. via MDM Privacy
  Preferences Policy Control profiles) before relying on the agent path. Note
  whether the prompts block the connection during this run.
- **Legacy VNC password.** Modern macOS requires the legacy VNC password option
  for non-ARD clients. The provisioner already passes
  `-setvnclegacy -vnclegacy yes`. Confirm that **noVNC / standard VNC clients**
  (not just Apple Screen Sharing.app) can authenticate with the 8-char password.
- **8-char password truncation.** The RFB DES scheme caps passwords at 8 bytes.
  Verify that the truncated password is exactly what the VNC client expects —
  enter only the first 8 characters if you supplied a longer one.
- **No-sudo behavior.** Re-confirm step 2: without sudo the command must surface
  `ErrDarwinSudoRequired`, never a generic failure.

## Result reporting

Record on issue #268: the macOS version, whether each step passed, the exact
`citadel vnc status` output before/after, whether noVNC connected with the legacy
password, and any TCC prompts encountered (and how they were resolved).
