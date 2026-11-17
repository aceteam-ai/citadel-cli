# VM console through a proxy node

Status: design only. No code ships with this document.

Refs #1601. The platform half is aceteam-ai/aceteam#13652 (the `vm_console_*`
MCP tools and the `node:vm_console` grant). aceteam-ai/aceteam#14122 (flows
that run desktop acceptance gates and capture docs screenshots) is the first
consumer.

This revises the #1601 design under Jason's decision of 2026-11-17. The
hypervisor is not enrolled as a Citadel node, and no root worker runs on it.
An existing non-root Citadel node proxies console access through the Proxmox
VE API. The first proxy is the operator's GPU workstation node, which runs a
user-service worker. The earlier transport (`qm` screendump and sendkey over a
root worker) is withdrawn.

## Open decisions for Jason

Each item gives the recommendation first, then the alternatives. The sections
below explain each one in detail.

1. **Where the PVE token secret lives on the proxy node.** Recommendation: an
   owner-only file under the node config dir
   (`<node config dir>/vm-console/secrets/<alias>.token`, mode 0600, in a
   0700 dir). The loader refuses it if group or other can read it, or if
   another user owns it. This is how the node already keeps its device token.
   Alternatives: seal it in `nodevault` and unseal it with the passcode
   presented in each job (rejected: nodevault has no unattended unlock, a
   6-digit master PIN is below `nodevault.AtRestThresholdBits`, so
   `UnlockAtRest` refuses it, and every job would run Argon2id); an OS
   keyring such as libsecret or `systemd-creds` (rejected for v1: a headless
   user unit has no reliable keyring, and it varies by OS).
2. **How the PVE endpoint's TLS is pinned.** Recommendation: pin the
   cluster's root CA (the PVE root CA file the operator copies from the
   host), with hostname verification. That pin survives node certificate
   renewal. Alternatives: pin the leaf certificate's SHA-256 fingerprint
   (easy to copy from the PVE UI, but renewal breaks it, and then the node
   refuses until the operator re-pins); system roots, for a host that serves
   an ACME certificate. All three are offered, and the CA pin is the
   documented default. Skipping verification is never offered.
3. **Does the node passcode gate VM console jobs?** Recommendation: yes,
   with the same semantics as the desktop gate (`jobs.DesktopGate`). All
   three job types are checked when a passcode or master PIN is set, and they
   share the node's wrong-passcode throttle. The cost: a proxy node with a
   passcode set (likely, if Shell or Desktop is on there) needs the passcode
   in every VM console job, unattended #14122 flow runs included. So the
   platform must either hold the passcode or ask a human for each run.
   Alternatives: require a configured passcode for keys and pointer and
   refuse when none is set (stricter, but different from every other
   surface); no passcode, relying on the permission, the allowlist and the
   platform grant, which leaves unattended flows simple.
4. **Can the platform turn the permission on remotely?** Recommendation: no.
   `vm_console` is enabled only by a local command on the proxy node.
   `APPLY_DEVICE_CONFIG` and the `citadel_set_*_permission` tools can turn it
   off but never on. Alternative: a remote toggle like Desktop and Files.
   Turning the permission on alone gives nothing, because the token and the
   allowlist are local. Refusing remote enable keeps a compromised platform
   from re-arming a surface the operator switched off.
5. **How narrow the `node:vm_console` grant is.** Recommendation: one grant
   per proxy node, as #13652 already specifies. The node-side allowlist
   bounds which VMs it reaches, and every audit row carries the host alias
   and vmid. Alternative: a grant scoped to (proxy node, host alias, vmid),
   which needs a richer grant scope string on the platform.
6. **Typing text.** Recommendation: no `VM_CONSOLE_TYPE` job in v1.
   `VM_CONSOLE_KEYS` takes a bounded sequence of chords, and the node owns
   pacing. A platform `vm_console_type` tool may compile text into a
   sequence after its secret-shaped input check (#14122 item 4).
   Alternatives: a dedicated type job on the node (it would duplicate the
   platform's secret check); a single chord per job (slow, and pacing would
   fall back to every caller).
7. **How an absolute pointer is detected.** Recommendation: in the RFB
   session itself. QEMU's pointer-type-change pseudo-encoding reports
   whether the guest's active pointer is absolute. The node refuses the
   pointer job unless the report says absolute. The token keeps `VM.Console`
   only. Alternative: add `VM.Audit` so the node can read the VM config's
   `tablet` flag (rejected: it widens the token to every config field, and
   the flag says nothing about whether the guest driver is active).
8. **Session lifetime.** Recommendation: one `vncproxy` session per job,
   opened and closed inside the handler. Alternative: hold a session per VM
   for a short idle time to save the roughly one second each connect takes.
   A held session keeps a live console socket on the proxy between jobs,
   which complicates revocation and the per-VM lock.
9. **How the platform learns what a proxy node offers.** Recommendation: the
   heartbeat advertises each host alias and its allowlisted vmids, with a
   view-only flag per VM. The target is the triple (proxy node, host alias,
   vmid). Alternative: a platform-side hypervisor registry, which needs DDL
   that #13652 rules out.

## Decision recap (Jason, 2026-11-17)

These are fixed and not reopened here.

- The Proxmox host is not a Citadel node and runs no Citadel worker.
- An existing non-root node proxies console access through the Proxmox API,
  using a dedicated API token whose role grants only `VM.Console` on the
  allowlisted VMs. That means no root, no shell and no `VM.Monitor`. The
  token is revocable in the Proxmox UI. It is stored node-side and never
  appears in job payloads or logs.
- Transport: `vncproxy` plus `vncwebsocket`, and an RFB client in the node.
  A screenshot is a framebuffer capture. Keys are RFB key events. The
  pointer is an RFB pointer event with absolute coordinates.
- Kept from the earlier design: console off by default; an allowlist, now
  per host and VM; `node:vm_console` with a 15-minute approval window for
  keys and pointer; no typed text in audit; agents never type secrets;
  refuse rather than guess for the absolute pointer.

## Findings from the code

These come from reading the current tree and the Proxmox API schema. The
first slice that touches each area confirms it with a test before relying on
it.

- **There is no RFB client to reuse.** The brief assumed one. The tree has
  an RFB 3.8 server (`desktop.VNCServer`, `internal/desktop/vnc_server.go`)
  with Raw and ZRLE encoders (`vnc_encode.go`), a WebSocket-to-TCP pump for
  browser noVNC (`internal/websockify`), and the DES bit-reversal helper
  `reverseBits` in `internal/platform/vnc.go`. A client is new code. It
  should share what exists: the server's wire constants and pixel-format
  handling, `reverseBits`, and the vendored gorilla/websocket. Its fake-server
  tests can borrow the server's message builders (`MarshalServerInit`,
  `MarshalFramebufferUpdateHeader`).
- **The existing VNC_KEYS vocabulary is xdotool names, not a keysym table.**
  `jobs.KeysHandler` joins the `keys` array into an xdotool combo, and
  `desktop.ParseActions` checks it against `safeKeyPattern`. The only
  numeric keysym mapping is `keysymToVK` in `input_windows.go`. It runs the
  other way (keysym to Windows virtual key) and is Windows-only. So the VM
  console reuses the name vocabulary that the platform's `vnc_keys` tool
  already emits, plus the same validation, and adds a new name-to-keysym
  table.
- **The existing Proxmox client and config must not be reused.**
  `proxmox.NewClient` defaults to `InsecureSkipVerify: true`, and its error
  path puts the response body into the error string. `proxmox.json` holds a
  provisioning token for the `INSTANCE_*` path, and that token's role is far
  broader than `VM.Console`. The console needs its own config, its own
  secret files and its own HTTP client.
- **The desktop handlers use `context.Background()` and log the key combo.**
  `KeysHandler` logs `VNC_KEYS sending %q`. The new handlers take the job
  context and log counts only.
- **`jobprivacy` only redacts the passcode, the profile PIN and staged
  bytes.** A `sequence` array of keysym names would reach a dead-letter
  entry and the job JSON renderings in plaintext. The keys job needs a
  per-kind redaction entry, the way `FILE_FETCH` has one
  (`jobprivacy.Payload`).
- **Privileged job types gate on the per-node stream** through
  `worker.isPerNodeStream`, as `AGENT_UPDATE`, `EXPOSE_SET` and `APP_*` do.
- **Refusals already have a terminal shape.** A `*jobs.ShellRefusal`
  (`{"reason","message"}`) is terminal on its first delivery and never
  retried. The VM console refusals reuse it.
- **The Proxmox API needs only `VM.Console` for both calls.** From the PVE
  API schema: `POST /nodes/{node}/qemu/{vmid}/vncproxy` checks
  `perm /vms/{vmid} [VM.Console]`. `GET /nodes/{node}/qemu/{vmid}/vncwebsocket`
  checks the same, and it also needs the `vncticket` from the vncproxy call.
  vncproxy returns `port`, `ticket`, `password`, `cert`, `upid` and `user`.
  `password` is described as the "password used for authentication within
  the VNC protocol", in printable ASCII. `generate-password` is deprecated:
  PVE now generates a password when one is required.
- **The VNC password is per display, and it expires after 30 seconds.** In
  the PVE source, `qm vncproxy` sets the password on the VM's QEMU display
  over QMP and expires it after 30 s. The vncproxy listener waits 10 s for
  its single connection. Any other console opener overwrites the password,
  so auth can race a human opening the PVE console (section 4.1).
- **The same token cannot read the VM config or locate the VM.**
  `GET /nodes/{node}/qemu/{vmid}/config` and `.../status/current` need
  `VM.Audit`. `GET /cluster/resources` is open to all users but is filtered
  by privilege. So with `VM.Console` alone, the node cannot discover which
  PVE node a VM is on, or read its `tablet` flag. Each allowlist entry names
  the PVE node, and pointer mode is detected inside RFB.
- **A token can read its own effective privileges.** `GET /access/permissions`
  lets every user or token dump its own permissions. The setup command uses
  this to refuse a token that holds anything beyond `VM.Console` on the
  allowlisted VM paths.
- **QEMU reports pointer mode in RFB.** In QEMU's `ui/vnc.c`, a client that
  lists pseudo-encoding -257 (`VNC_ENCODING_POINTER_TYPE_CHANGE`) in
  SetEncodings gets a FramebufferUpdate rect with that encoding at once, and
  again whenever the mode changes. Its x field is 1 for absolute, 0 for
  relative. In absolute mode, `pointer_event` maps x and y onto the
  framebuffer width and height. In relative mode, it turns them into deltas,
  which is exactly the guess the refusal rule forbids.
- **QEMU never sends CopyRect.** Its SetEncodings parser handles Raw,
  Hextile, Tight, Tight PNG, Zlib, ZRLE, ZYWRLE, DesktopResize and the
  extended resize, but no CopyRect. Raw is therefore the floor.
- **The node already advertises live job types** through
  `Runner.SupportedJobTypes`, published as `capabilities.job_types`.

## Overview

```
platform (vm_console_* tool, node:vm_console grant, audit)
   |  VM_CONSOLE_* job on proxy node M's own stream: {host: alias, vmid: N}
   v
proxy node M (non-root user-service worker)
   gates: per-node stream, vm_console permission, passcode, allowlist,
          input flag, per-VM lock
   config: alias -> PVE URL, TLS pin, PVE node name, token id
   secret: owner-only token file
   |  HTTPS, pinned:  POST .../qemu/N/vncproxy        (VM.Console)
   |  WSS, pinned:    GET  .../qemu/N/vncwebsocket    (VM.Console)
   v
Proxmox VE API on the hypervisor
   |
   v
QEMU VNC server for VM N  (RFB: framebuffer, key events, pointer events)
```

The platform never learns the hypervisor's address or the token. It names a
host alias that only the proxy node can resolve.

## 1. Configuration

### 1.1 What the operator creates in Proxmox

One PVE user and one role for all console proxies, and one token per proxy
node. An ACL entry per VM goes to both the user and the token. With
`privsep=1`, the token's effective rights are the intersection of its own ACL
and its user's, so both need the entry.

```sh
# Once per cluster.
pveum role add CitadelConsole --privs VM.Console
pveum user add citadel-console@pve --comment "Citadel console proxy, no login"

# Once per proxy node. The token id names the proxy so the PVE task log and
# revocation are per proxy. --expire is seconds since the epoch (about 90 days).
pveum user token add citadel-console@pve <proxy-alias> --privsep 1 \
    --expire <epoch> --comment "console proxy"

# Once per allowlisted VM (example: vmid 108).
pveum acl modify /vms/108 --users citadel-console@pve --roles CitadelConsole --propagate 0
pveum acl modify /vms/108 --tokens 'citadel-console@pve!<proxy-alias>' \
    --roles CitadelConsole --propagate 0
```

Rules the operator guide states plainly:

- The user gets no password, so it cannot log in to the PVE UI.
- ACLs go on `/vms/<vmid>` only. Not on `/`, not on `/vms`, and not on a
  pool. A pool ACL silently grants console on every VM later added to it.
- The role has exactly `VM.Console`. Not `VM.Monitor`, not `VM.PowerMgmt`,
  not `VM.Audit`, not `Sys.*`.
- The token has an expiry.

`VM.Console` is enough for both calls the node makes (see Findings). It is
still the full console: whatever a person at that VM's keyboard and screen
can do, the token can do, including Ctrl+Alt+Delete and the boot menu. That
is the blast radius the rest of this design bounds.

Every `vncproxy` call creates a PVE task owned by the token's user, so the
hypervisor keeps its own record of every console session, independent of
Citadel.

### 1.2 Files on the proxy node

Everything lives under `network.GetNodeConfigDir()`, the machine-convergent
node config dir, not `platform.ConfigDir()`. The worker and the operator's
shell must resolve the same files (see the `ConfigDir()` note in CLAUDE.md).

```
<node config dir>/vm-console/            0700
<node config dir>/vm-console/hosts.yaml  0600   no secrets
<node config dir>/vm-console/secrets/    0700
<node config dir>/vm-console/secrets/<alias>.token  0600  token secret only
```

`hosts.yaml`:

```yaml
hosts:
  - alias: lab-pve                 # [a-z0-9-]{1,32}, the only name the platform sees
    url: https://<pve-address>:8006  # https only, no path, no userinfo
    tls:
      mode: ca                     # ca | fingerprint | system
      ca_file: vm-console/lab-pve-ca.pem
      server_name: ""              # optional SNI and verify name override
    token_id: citadel-console@pve!workstation
    token_expires: 2027-02-15      # informational; status warns 14 days ahead
    default_pve_node: <pve-node>
    vms:
      - vmid: 108
        label: windows-test        # optional, shown to the owner org
        pve_node: ""               # empty means default_pve_node
        input: true                # false = screenshot only
```

The loader (`vmconsole.LoadConfig`, new) is strict. It refuses the whole file
if it does not parse, has an unknown key, has a duplicate alias or a
duplicate vmid within one alias, or has a URL that is not `https` with a host
and optional port. A refused file means no VM is reachable, never "last good
config". The token file loader refuses a file that group or other can read
or write, or that another user owns, or a symlink. It reads at most 256
bytes and trims one trailing newline.

Both files are read per job, like the live permission gate. Removing a VM or
a host takes effect on the next job without a worker restart.

Windows proxies are out of scope for v1. The owner-only check needs a DACL
check, as `localviewer` and the co-browse base dir already do, and the first
proxy is Linux. On Windows the handlers are not registered, and the refusal
says the platform is unsupported.

### 1.3 Commands

All of these are local and run as the worker's user. The secret is read from
stdin, never from argv or the environment, so it never lands in shell history
or `/proc/<pid>/cmdline`.

```sh
citadel vm-console host add <alias> --url https://... --pve-node <name> \
    --token-id 'user@pve!name' (--ca-file PATH | --fingerprint SHA256 | --system-roots) \
    [--token-expires YYYY-MM-DD] < token-secret
citadel vm-console host rotate-token <alias> < new-token-secret
citadel vm-console host remove <alias>      # deletes the secret file too
citadel vm-console vm add <alias> <vmid> [--pve-node NAME] [--view-only] [--label TEXT]
citadel vm-console vm remove <alias> <vmid>
citadel vm-console status                   # config, pins, expiry warnings, no secrets
citadel vm-console test <alias> <vmid>      # see below
citadel permission vm-console <enable|disable>
```

`host add` and `rotate-token` verify before they save:

1. TLS against the chosen pin. A failure prints the presented leaf
   fingerprint, so the operator can compare it with the PVE UI, and does not
   save.
2. `GET /version`, to confirm the URL is a PVE API.
3. `GET /access/permissions` as the token. Save is refused if the token
   holds any privilege other than `VM.Console`, or holds it on any path other
   than `/vms/<vmid>`. This catches a role or ACL that was set up too wide.
   The refusal names the extra privilege and path.

`vm add` repeats step 3 for the new vmid and warns, but still saves, if the
token lacks `VM.Console` there. The operator may add the ACL afterwards.

`test` runs one real session: `vncproxy`, the websocket, the RFB handshake,
one framebuffer update. It prints the framebuffer size, the pointer mode
(absolute, relative or not reported) and the PVE task id. It writes no image
to disk and sends no input. This is the operator's pre-flight for the tablet
device.

### 1.4 Rotation and revocation

- **Rotate:** create a new token in PVE, add the two ACL entries for it, run
  `citadel vm-console host rotate-token`, then remove the old token in PVE.
  The new secret is written atomically (temp file in the same dir, fsync,
  rename) so a job never reads half a token.
- **Revoke at the hypervisor:** remove the token, or delete one VM's ACL
  entry. The next job fails with `pve_auth_refused` or `pve_forbidden`.
  Because sessions are per job (decision 8), nothing outlives revocation by
  more than one job's budget.
- **Revoke at the node:** `citadel vm-console host remove`, `vm remove`, or
  `citadel permission vm-console disable`. All take effect on the next job.
- **Expiry:** `status` and the worker's startup log warn 14 days before
  `token_expires`. An expired token fails as `pve_auth_refused` with a
  message that names rotation.

### 1.5 TLS

The console client builds its own `tls.Config` from the host entry. That
config is shared by the HTTP client and the websocket dialer, so the pin
covers both legs.

- `ca`: `RootCAs` holds only the pinned CA, and normal verification runs,
  including the hostname check against `server_name` or the URL host. PVE
  node certificates are signed by the cluster's own root CA, which lasts for
  years. A routine node certificate renewal therefore keeps working.
- `fingerprint`: `InsecureSkipVerify` plus a `VerifyConnection` callback that
  compares the leaf's SHA-256 with the pin in constant time. It refuses on
  any mismatch. This is the only place the flag is set, and a test pins that
  the callback is always present when it is.
- `system`: system roots plus the hostname check, for an ACME certificate.

In every mode the client refuses redirects, ignores proxy environment
variables (`Proxy: nil`), sets TLS 1.2 as the minimum, and bounds the size
of response bodies. Error text names the status and a fixed reason, never
the response body.

## 2. The node-side gate

### 2.1 A new permission, default off

`config.Permissions` gains `VMConsole bool` (`vm_console` in YAML),
default-deny, and `IsSensitiveCategory` gains `"vm_console"`. It is separate
from `Desktop`. Enabling Desktop does not enable VM console, and enabling VM
console does not enable Desktop. The VM console handlers never touch
`internal/desktop`'s local capture or input. A proxy node's own screen stays
behind its own gate.

`Console` already names terminal access, so the new field is `VMConsole`,
and the platform setter would be `citadel_set_vm_console_permission`
(disable only, per decision 4).

### 2.2 Gate order

Each handler checks these in order, and refuses with a typed
`*jobs.ShellRefusal` at the first failure. A refusal is terminal on its first
delivery.

1. Per-node stream (`isPerNodeStream`). Otherwise `not_per_node_stream`.
2. `vm_console` permission, live. Otherwise `vm_console_disabled`.
3. Supported platform (Linux or macOS). Otherwise `unsupported_os`.
4. Passcode, through the desktop gate semantics (decision 3), with the shared
   `jobs.PasscodeThrottle`. The usual passcode reasons apply.
5. Config loads strictly. Otherwise `vm_console_misconfigured`, with a fixed
   message that says which file to check, and no file contents.
6. `(host, vmid)` is in the allowlist. Otherwise `vm_not_allowlisted`. An
   unknown alias and a known alias with an unlisted vmid give the same
   reason and message.
7. For keys and pointer, the entry has `input: true`. Otherwise
   `vm_view_only`.
8. Payload validation (section 3). Otherwise `invalid_payload`.
9. The per-VM lock (section 3.5). Otherwise `vm_console_busy`.
10. The token file loads strictly. Otherwise `vm_console_misconfigured`.

The allowlist is the node-side boundary. The platform's grant and its
15-minute window are enforced by the platform only, because the node has no
way to verify a platform grant. The operator's node-side control over input
is `input: false` per VM, which leaves a VM screenshot-only whatever the
platform decides.

### 2.3 Registration and advertisement

The three handlers are registered through the same live-gate pattern as
Desktop and Files (`CreateLegacyHandlersWithOpts` options with an enabled
callback), and `registrationRefusals` names the reason when they are
withheld. They are native `worker.JobHandler`s, not legacy adapters, so they
get the job context and typed payloads.

`capabilities.job_types` lists the three types only when the permission is
on and at least one VM is allowlisted. The heartbeat gains an additive,
omitempty `vm_console` object:

```json
{"vm_console": {"hosts": [
  {"alias": "lab-pve", "vms": [{"vmid": 108, "label": "windows-test", "input": true}]}
]}}
```

It carries no URL, no PVE node name, no token id and no expiry.

## 3. Job contract

### 3.1 Addressing

The platform addresses "VM N via proxy node M" as the triple
`(node_id M, host alias, vmid N)`. It dispatches to M's own per-node stream
with `{"host": "<alias>", "vmid": N}`. A payload never carries a URL, a PVE
node name or any credential. If the payload could name an endpoint, a
compromised platform could point the node's token at a server it controls.

The platform builds its target list from the heartbeat advertisement
(decision 9). If two proxy nodes advertise the same alias and vmid, the tool
requires the caller to name the node. The `node:vm_console` grant is held on
node M.

### 3.2 Common fields and limits

| Field | Type | Rule |
|---|---|---|
| `host` | string | `^[a-z0-9-]{1,32}$` |
| `vmid` | integer | 100 to 999999999 (PVE's range) |
| `passcode` | string | optional; the existing wire key; never logged |

Every job has its own budget below the platform's wait: 20 s for a
screenshot, 25 s for keys and pointer. The budget covers the API call, the
websocket, the RFB handshake and the action. The platform tool must wait at
least 30 s (a note for #13652). These job types take the default watchdog
tier and the ordinary dispatch path. They are not added to
`serializedLaneJobTypes`, because that exec-1 lane is shared with model pulls
and a click would queue behind a long download.

### 3.3 VM_CONSOLE_SCREENSHOT

Payload: common fields, plus optional `max_width` and `max_height` (64 to
3840 and 64 to 2160). The image is downscaled to fit, keeping its aspect
ratio, and never upscaled.

Result:

```json
{
  "image": "<base64 PNG>", "encoding": "base64", "format": "png",
  "width": 1280, "height": 800,
  "fb_width": 1280, "fb_height": 800,
  "pointer": "absolute",
  "host": "lab-pve", "vmid": 108,
  "captured_at": "2026-11-17T20:00:00Z",
  "sha256": "<hex of the PNG bytes>"
}
```

`width` and `height` are integers (the existing desktop screenshot returns
strings; this contract does not copy that). `pointer` is `absolute`,
`relative` or `unknown`. Limits: a framebuffer above 4096 by 4096 is refused
(`framebuffer_too_large`), and an encoded PNG above 8 MiB is refused
(`image_too_large`). The caller can retry with `max_width`. The image lives
in memory only: it is never written to disk on the node and never logged.

### 3.4 VM_CONSOLE_KEYS

Payload: common fields, plus:

- `sequence`: an array of 1 to 256 chords. Each chord is an array of 1 to 4
  key names from the VNC_KEYS vocabulary (X keysym names such as `ctrl`,
  `alt`, `shift`, `super`, `Return`, `Tab`, `Escape`, `F1`, `a`). A name
  outside the closed table is refused as `invalid_payload`, naming its index
  only, never the name.
- `keys`: a single chord, accepted as shorthand for a one-chord sequence, to
  match VNC_KEYS. Sending both fields is refused.
- `pace_ms`: optional, 20 to 250, default 40. The gap between chords.

Execution: for each chord, press the keys in order and release them in
reverse. Within a chord, the node waits 10 ms between events. On any error,
cancellation or budget expiry partway through, the node sends a key-up for
every key it holds before it closes the session, so a modifier is never left
stuck on the guest. The total of the sequence length times `pace_ms` must
fit the budget, or the job is refused before it connects
(`sequence_too_long`).

Result: `{"ok": true, "chords": N, "keys": M}`. If the job fails partway,
the error says how many chords were sent, so the caller knows input may have
reached the guest. Nothing is retried.

### 3.5 VM_CONSOLE_POINTER

Payload: common fields, plus:

- `action`: `move`, `click`, `double_click` or `drag`.
- `button`: `left`, `middle` or `right`; default `left`; ignored for `move`.
- `x`, `y`: integers, the point in screenshot pixels.
- `to_x`, `to_y`: the end point, for `drag` only.
- `ref_width`, `ref_height`: the `width` and `height` of the screenshot the
  coordinates refer to.
- `fb_width`, `fb_height`: that screenshot's `fb_width` and `fb_height`.

There are no separate `down` and `up` actions. Each job is its own RFB
session (decision 8), and a button held across jobs would depend on how the
hypervisor treats a disconnect with a button down. A drag is one job: press
at the start, move in up to 16 steps, release at the end.

Rules:

- Every coordinate is an integer with `0 <= x < ref_width` and
  `0 <= y < ref_height`. Otherwise `invalid_payload`.
- Scaling to the framebuffer: `fx = floor(x * fb_width / ref_width)`, and the
  same for y, clamped to the framebuffer. A pure function with table tests.
- If the live framebuffer size differs from `fb_width` and `fb_height`, the
  job is refused (`resolution_changed`). The guest changed resolution since
  the screenshot, and the click would land somewhere else.
- If the session has not reported absolute pointer mode (section 4.4), the
  job is refused (`absolute_pointer_unavailable`) with the message "absolute
  pointer unavailable on vmid N; enable the tablet device". There is never a
  relative fallback. Enabling the tablet is an owner VM config change, never
  done by Citadel.
- On error or cancellation, the node sends a pointer event with no buttons
  before it closes.

Result: `{"ok": true, "action": "click", "fb_x": 640, "fb_y": 400}`.

### 3.6 Per-VM lock and concurrency

A process-wide lock keyed by `(alias, vmid)` serializes jobs for one VM, so
a keys job and a pointer job never interleave input. It is a `TryLock` with a
2 s wait, then `vm_console_busy`. Separately, at most 4 console sessions run
at once per proxy node, across all VMs.

### 3.7 Privacy plumbing

`jobprivacy` gains a per-kind entry for `VM_CONSOLE_KEYS` that drops
`sequence` and `keys`, so neither reaches a dead-letter entry, a log line or
the job JSON renderings. Handler logs carry the job id, type, alias, vmid,
outcome, reason, chord and key counts, pointer action and framebuffer
coordinates, image size and SHA-256, the PVE task id and the duration.
Nothing else.

## 4. RFB specifics

### 4.1 Session setup

What PVE does, read from its source (qemu-server `PVE/API2/Qemu.pm`
`vncproxy`, qemu-server `PVE/CLI/qm.pm` `vncproxy`, and pve-http-server
`PVE/APIServer/AnyEvent.pm`):

- `vncproxy` with `websocket=1` generates a VNC password and returns it as
  `password`. It also returns `ticket` as `<password>:<vnc ticket>`, which is
  the value `vncwebsocket` expects as `vncticket`.
- It then starts a task that listens on `localhost:<port>`, without TLS, and
  waits **10 seconds** for one connection. The `vncwebsocket` upgrade is that
  connection, so the node must open the websocket within 10 s of the API
  reply.
- On that connection, PVE runs `qm vncproxy <vmid>`. That command sets the
  VM display's VNC password over QMP (`set_password`, protocol vnc), expires
  it after **30 seconds** (`expire_password +30`), and pipes the QEMU VNC
  socket through. QEMU's VNC server therefore asks for VNC Authentication,
  with the returned `password` as the secret.
- The VNC password is per display, not per session. Every console opener
  overwrites it, the PVE web console included. If a human opens the console
  for the same VM between the node's websocket connect and its RFB auth, the
  node's auth fails. The reverse also applies: the node's connect can fail a
  human's auth that is in flight at that moment. An established session is
  unaffected, because the password only gates new authentications.
- The websocket upgrade goes through the same authentication handler as
  every other API call, and that handler accepts the `PVEAPIToken`
  Authorization header. The server echoes `Sec-WebSocket-Protocol` if one is
  sent and does not require it. It sends and expects binary frames, and it
  refuses fragmented, unmasked or oversized client frames.
- The request may go to any cluster node's API. `{node}` must be the node
  that hosts the VM: `vncproxy` loads the VM config on that node, and a
  migrated VM fails there with an error.

The client's steps:

1. `POST /api2/json/nodes/<pve_node>/qemu/<vmid>/vncproxy` with
   `websocket=1`, authenticated with `Authorization: PVEAPIToken=<id>=<secret>`.
   Keep `port`, `ticket`, `password` and `upid`. Do not send
   `generate-password`, which is deprecated. A 401 maps to
   `pve_auth_refused`, a 403 to `pve_forbidden`, and a config-not-found error
   to `vm_not_on_pve_node` (the VM migrated, so the operator must update
   `pve_node`).
2. At once, and within the 10 s listener window, open
   `wss://<host>/api2/json/nodes/<pve_node>/qemu/<vmid>/vncwebsocket?port=<port>&vncticket=<urlencoded ticket>`
   with the same Authorization header, the pinned TLS config, a 5 s
   handshake timeout and the `binary` subprotocol (accepted, not required).
   Each binary message carries RFB bytes. A text message is a protocol error.
3. RFB: read the server version and reply `RFB 003.008`. From the security
   types, choose VNC Authentication (2) and answer the 16-byte challenge with
   DES keyed by the bit-reversed `password` (its first 8 bytes,
   zero-padded). Refuse to proceed with security type None: a console that
   asks for no auth is not the session vncproxy promised. Read
   SecurityResult.
4. If SecurityResult reports failure, retry once from step 1, with a fresh
   `vncproxy` call, after a 500 ms pause. If the retry fails too, refuse with
   `vnc_auth_raced` and a message that says another console may be opening
   the same VM. A failed RFB auth is never reported as a revoked token: only
   an API 401 or 403 means that.
5. ClientInit with **shared = 1**, so the session never disconnects a human
   who is watching the same console in the PVE UI.
6. Read ServerInit (framebuffer size, pixel format, name). Bound the name at
   256 bytes.
7. SetPixelFormat: 32 bpp, depth 24, true colour, little-endian,
   8-bit channels at shifts 16, 8, 0.
8. SetEncodings: Raw (0), DesktopSize (-223), PointerTypeChange (-257).
   Nothing else in v1.

Secrets in this sequence (the token, the ticket, the VNC password) live only
in local variables for the session and are never formatted into errors or
logs. The ticket carries the VNC password and travels in the websocket URL,
so the dialer's error path must not echo the URL. A test greps every error
and log line for a sentinel ticket.

### 4.2 Framebuffer

A screenshot sends FramebufferUpdateRequest (non-incremental, full screen)
and reads updates until every pixel of the framebuffer has been covered once,
or the budget ends. Raw rects are decoded into an RGBA buffer. A DesktopSize
rect resizes the buffer and starts the coverage over. A PointerTypeChange
rect updates the pointer mode. Any other encoding is a protocol error,
because the server was told only these three. Every rect is bounds-checked
against the framebuffer before its bytes are read, and a rect whose payload
would exceed the 4096 by 4096 cap is refused before any allocation. All
reads carry deadlines.

Raw at 32 bpp is about 4 MB for a 1280 by 800 screen and about 8 MB for
1920 by 1080, over the LAN websocket. That is acceptable for one screenshot
per job. ZRLE would cut it a lot, but it needs a decoder (the tree has only
an encoder), so it is a later slice once a measured need exists.

Downscaling, when requested, uses `golang.org/x/image/draw` (already a
dependency) with an area-averaging scaler. PNG encoding uses the standard
library.

### 4.3 Keys

KeyEvent carries an X keysym. The new table (`rfb.KeysymByName`) maps the
VNC_KEYS vocabulary to keysym values: letters and digits as their ASCII
keysyms, and named keys such as `Return` (0xff0d), `Tab` (0xff09),
`Escape` (0xff1b), `BackSpace` (0xff08), `Delete` (0xffff), the arrows,
`Home`, `End`, `Page_Up`, `Page_Down`, `F1` to `F12`, and the modifiers
`ctrl`/`Control_L` (0xffe3), `shift`/`Shift_L` (0xffe1), `alt`/`Alt_L`
(0xffe9) and `super`/`Super_L` (0xffeb). The table is closed. A golden test
pins it to the names that the platform's `vnc_keys` tool emits.

QEMU converts keysyms to scancodes with the VM's keymap, which defaults to
en-us. A guest using a different layout may receive a different character
for a printable key. v1 documents this. The layout-independent route is
QEMU's extended key event (-258), which sends a scancode with each key. It is
a later slice, and it is how the PVE web console avoids the problem.

### 4.4 Absolute pointer

Because the client lists -257 in SetEncodings, QEMU answers at once with a
PointerTypeChange rect whose x field is 1 (absolute) or 0 (relative), and
sends another whenever the mode changes, for example when the guest's tablet
driver loads. The client keeps the latest value.

The pointer handler:

1. After SetEncodings, reads updates for up to 2 s until it has seen a
   PointerTypeChange rect. If none arrives, mode is `unknown`, and the job
   is refused. A server that does not report its mode is treated as not
   absolute.
2. Refuses unless the mode is absolute.
3. Reads any pending updates right before each pointer event, and refuses
   mid-action if the mode changed. Before closing, it sends a no-buttons
   event.

In absolute mode QEMU maps the RFB x and y onto the framebuffer width and
height, so `fx` and `fy` from section 3.5 are sent unchanged. Button mask:
left 1, middle 2, right 4. A click is press then release, 30 ms apart. A
double click is two clicks 80 ms apart.

### 4.5 Builder checks

Section 4.1 settles three questions from the PVE source: which secret the
VNC auth uses, whether a token header works on the websocket upgrade, and
how long the listener waits. The questions below remain open. A fake server
only encodes an assumption and cannot confirm one, so the native checkpoint
in section 6 answers them before the input slices start.

1. That QEMU on the host offers only VNC Authentication on this path, with
   no VeNCrypt. The client supports only VNC Authentication. If the
   checkpoint shows otherwise, that is a design change and comes back here.
2. That the PVE node certificate's SANs include the name or address the
   operator configures. If not, `server_name` covers it.
3. That a shared-flag session leaves a human's open PVE console connected.
4. That the -257 rect arrives on the host's QEMU version, and reports
   absolute for a Windows guest with the default USB tablet.
5. How often the per-display password race (section 4.1) shows up in
   practice when a human has the PVE console open. The retry should absorb
   it.
6. What the guest sees if the session drops with a key or button held. The
   release-before-close rule makes this rare, but acceptance should note it.

## 5. Threats

| Threat | What bounds it |
|---|---|
| Token stolen from the proxy node (malware as the worker's user, a backup, a stolen disk) | The role is `VM.Console` on the listed `/vms/<vmid>` paths, checked at setup through `/access/permissions`. No power, config, monitor or shell rights. Expiry, per-proxy tokens, one-command revocation in PVE, and the PVE task log attributes every session to the token. The file is owner-only, and the loader refuses it otherwise. The residual risk is the full console of the listed VMs, which is accepted. A local attacker running as the worker's user already holds the node's device token. |
| Malicious or compromised platform job targets a VM not on the allowlist | The allowlist is local, read per job, and keyed by alias and vmid. The payload cannot carry an endpoint, a PVE node or a credential. The token's PVE ACL is a second, independent boundary on the hypervisor. |
| Compromised platform re-enables a console the operator switched off | Remote enable is refused (decision 4). The token and allowlist are local, and `input: false` per VM is local. |
| Compromised platform drives input inside the 15-minute window it controls | The window is platform-enforced. The node can only bound input per VM (`input: false`), require the passcode (decision 3), and log every action locally with counts and coordinates. |
| Token or ticket sent to the wrong server | The TLS pin is shared by the HTTP and websocket legs, redirects and proxies are refused, and the URL is local config, never payload. |
| Secrets in logs or errors | Response bodies and URLs are never echoed. Sentinel tests cover the token, the ticket, the VNC password, the passcode and a keys sequence. |
| Screenshots show secrets on the guest's screen | Images live in memory only on the node and are never logged. The size cap and optional downscale apply. Retention is a platform concern: by default the platform keeps no screenshot beyond the tool result unless a flow's evidence bundle (#14122) asks for one. Pixel redaction is not attempted, because it cannot be done reliably. Per-VM masking rectangles are a possible later slice. Agents never type secrets, and the operator does secret entry, such as a login, by hand in the PVE console. |
| Keystroke content in audit | No typed text anywhere: `jobprivacy` drops `sequence` and `keys`, handler logs carry counts, and platform audit (#13652) carries counts. A hash of typed text is not recorded either, because short text is cheap to brute-force from a hash. |
| Two jobs interleaving input on one VM | The per-VM lock (section 3.6). |
| A human's console and the node race on the per-display VNC password | One retry with a fresh vncproxy call, then `vnc_auth_raced`. It is never reported as a revoked token. The shared flag keeps an established human session connected. |
| Stuck modifier or button after a failure | Release-before-close on every exit path, pinned by tests. |
| Hostile or broken RFB server (memory, CPU) | Bounded rects and framebuffer, deadlines on every read, a closed set of encodings, and a per-node session cap. |
| Interaction with the desktop gate | Separate permission and separate handlers, with no shared capture or input path. The passcode throttle is shared on purpose, so wrong guesses on either surface count together. |

Audit fields (node logs and the platform's `agent_action_audit` alike): job
id, job type, proxy node, host alias, vmid, outcome, refusal reason, PVE task
id, duration, chord and key counts (keys), action, button and framebuffer
coordinates (pointer), image dimensions, byte size and SHA-256 (screenshot).
The SHA-256 lets #14122's evidence bundle tie a stored screenshot to the job
that produced it.

## 6. Slices

In dependency order. Each is sized for one agent. CI has no hypervisor:
every test uses a fake PVE server (`httptest.NewTLSServer`, with its own
minted CA or leaf for the pin tests) and a scripted fake RFB server. Native
acceptance against the operator's Windows test VM (vmid 108) runs after
merge, per the standing rule.

**S1. RFB client core (`internal/rfb`).** Opus review: yes (parser of
untrusted input). The 3.8 handshake, VNC Authentication (sharing
`reverseBits`, moved to a small shared helper), the shared-flag ClientInit,
SetPixelFormat, SetEncodings, the Raw, DesktopSize and PointerTypeChange
decoders, KeyEvent and PointerEvent, deadlines, and size bounds. Transport is
an `io.ReadWriter`, so it is testable without websockets. Acceptance: a
scripted fake server covers a full screenshot round trip; a resize mid-update
restarts coverage; an unknown encoding, an oversized rect and a long name
are refused before allocation; security type None is refused; the pointer
mode is tracked across change rects; a `-race` test runs the read loop
alongside the event writes.

**S2. Keysym table and chord validation.** Sonnet review. `rfb.KeysymByName`,
chord and sequence validation, and the press/release ordering as a pure
function. Acceptance: a golden test of the vocabulary that `vnc_keys` emits;
unknown names are refused by index without echoing the name; reverse-order
release.

**S3. Console config and PVE client (`internal/vmconsole`).** Opus review:
yes (credential and TLS). The strict `hosts.yaml` loader, the owner-only
token loader, the three TLS modes, vncproxy, and the vncwebsocket dial into
an `io.ReadWriter` for S1. Acceptance: a fake PVE server checks the token
header, the `websocket=1` form and the ticket in the query; a CA pin passes
a renewed leaf and refuses a foreign CA; a fingerprint pin refuses any other
leaf; a redirect is refused; a proxy environment variable is ignored; a
group-readable or foreign-owned or symlinked token is refused; an error body
containing a sentinel never appears in an error; a URL with a ticket never
appears in a dial error; a 401 maps to `pve_auth_refused`, a 403 to
`pve_forbidden`, and a missing VM config to `vm_not_on_pve_node`; a failed
RFB SecurityResult triggers exactly one fresh vncproxy call, then
`vnc_auth_raced`; a websocket opened after the fake listener's 10 s window
fails with a bounded error.

**S4. Operator commands.** Sonnet review. `citadel vm-console ...` and
`citadel permission vm-console`, the `/access/permissions` check, the
atomic secret write, and `test`. Acceptance: the secret comes from stdin
only; a token with an extra privilege, or with `VM.Console` on `/vms` or a
pool, is refused with the privilege and path named; `status` prints no
secret; `remove` deletes the secret file; every command's tests run against
a `t.TempDir()` config dir, never the real node config dir.

**Native checkpoint, after S1, S3 and S4 merge and before S5 and S6
start.** The operator runs `citadel vm-console test` against the Windows test
VM. It is read-only: it sends no input and saves no image, so it falls under
the post-merge acceptance rule. It answers the builder checks in section 4.5
(the security type, the SANs, the shared session, the -257 report, the
password race) on the real host. If the transport assumption is wrong, two
Opus-reviewed handler slices are not yet built on it.

**S5. Permission, gate, screenshot handler, advertisement.** Opus review:
yes. `Permissions.VMConsole`, `IsSensitiveCategory`, refusal of remote
enable in `APPLY_DEVICE_CONFIG`, the gate order of section 2.2, the native
`VM_CONSOLE_SCREENSHOT` handler, registration, `registrationRefusals`,
`allKnownJobTypes`, the `capabilities.job_types` condition, the heartbeat
`vm_console` object and the per-VM lock. Acceptance: each gate step refuses
with its reason, in order; the shared stream is refused; a vmid not on the
allowlist is refused with the same message as an unknown alias; a remote
enable is refused while a remote disable works; downscale and the size caps
work; the heartbeat carries no URL, PVE node or token id; no image bytes
reach a log.

**S6. Keys and pointer handlers.** Opus review: yes (input on a real
machine). `VM_CONSOLE_KEYS` and `VM_CONSOLE_POINTER`, pacing, release on
error, the `jobprivacy` entry, the scaling function, the resolution check,
and the absolute-pointer refusal. Acceptance: keys and pointer are refused
for a view-only VM; a sequence that cannot fit the budget is refused before
connecting; cancellation partway through sends key-ups for every held key;
the pointer is refused when the mode is relative, when no mode report
arrives, and when the mode flips mid-drag; no relative event is ever sent
(the fake server fails the test if it sees a pointer event while relative);
scaling table tests cover odd sizes and edges; `resolution_changed` fires on
a size mismatch; the dead-letter and log sentinel tests cover `sequence`.

**S7. Operator guide and CLAUDE.md entry.** Sonnet review. Docs only: the
PVE setup from section 1.1, the commands, the builder checks with their
results once known, and a short CLAUDE.md paragraph naming the owning
functions.

Platform work in #13652 can start once S5's heartbeat shape and the payloads
in section 3 are accepted. It does not need to wait for the node slices to
merge.

## Out of scope

- Enrolling the hypervisor, or any root worker.
- Power, snapshot, config or guest-agent actions. These need other PVE
  privileges and are deliberately absent.
- LXC console (`termproxy`) and serial console.
- ZRLE or Tight decoding, extended key events, scroll, and per-VM masking.
  Each is a possible later slice with its own review.
- Windows as a proxy node.
- The platform's flow nodes, vision checkpoints and evidence bundles
  (#14122).
