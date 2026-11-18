# Local co-browse: file upload, and field editing parity

Status: design of record. S1 implements file-aware commit snapshots; S2 adds
the opt-out setting and shared gate. Local upload entry points remain in later
slices; the setting alone does not enable staging or attachment.

Refs #1729 (local-mode file upload) and #1730 (type with selector and clear,
key modifiers). Builds on #1721 / PR #1722 (staging, `set_file_input`, chooser
interception), #1723 / PR #1728 (human attach on the node path) and PR #1731
(`TypeWith`, `KeyWith`, `CheckKeyChord`, still in review). The platform side of
the edit contract is aceteam#13782.

## Decisions (Jason, 2026-11-17)

Where Jason chose differently from the design's first recommendation, that
recommendation is kept as the rejected alternative.

1. **File source: both.** The human can use a picker and drop zone in the
   local viewer page, and an OS file dialog that Citadel launches. Citadel
   reads the chosen path once and never accepts a path from the agent (Q1).
   Rejected alternative (first recommendation): the viewer picker and drop
   zone only.
2. **Staged bytes live in the platform's per-session staging dir**
   (`<session dir>/.citadel-stage`, under `DefaultCobrowseSessionsBaseDir`).
   Every teardown path already removes it. Rejected alternative: a dir under
   `localStateDir`, which nothing sweeps after a crash.
3. **Agent staging is allowed in v1, with acceptance.**
   `local_cobrowse_stage_file` takes base64 bytes. Nothing is staged until
   the human accepts a card showing name, size and sha256, and every attach
   still asks (Q2). Rejected alternative (first recommendation): no agent
   bytes in v1.
4. **Both the human and the agent attach in v1.** The human attaches from the
   viewer card without needing Take control. The agent attaches through
   `local_cobrowse_set_file_input` (Q3). Rejected alternative (first
   recommendation): human only in v1.
5. **Every agent attach asks.** It never rides a domain grant, and the
   approval binds the staged file's sha256, the page, the chooser and the
   frame URL. This is in v1. Rejected alternative (first recommendation):
   defer agent attach to a later slice.
6. **A pending file chooser does not block agent writes.** The agent keeps
   acting, and the attach fences refuse if the page moved. Q3 covers what the
   agent sees and how stale choosers are cleared. Rejected alternative (first
   recommendation): block agent writes while a chooser is pending, as a
   pending commit does.
7. **Forms with a file field become verifiable in v1.** Each file entry binds
   by field name, file name, size, type and lastModified. This slice can
   start first. Rejected alternative: leave such forms to the human.
8. **Choosers in a cross-site frame are refused in v1** with a clear reason.
   Rejected alternative: auto-attach to out-of-process frames, with a card
   naming the frame's site.
9. **Edit and chord consent as recommended.** `type` with selector or clear,
   and the allowed chords, ride the per-domain write grant. The listed edge
   cases ask (Q4). Hidden, password and one-time-code targets are refused for
   selector edits. Rejected alternatives: make every `clear` ask; allow
   password targets through selector.
10. **#1584 (CDP over a pipe) must land before the quickstart advertises
    uploads.** It does not block building. Rejected alternative: block all
    local upload work on #1584.
11. **Uploads have an opt-out.** They are on by default. `citadel local config
    uploads off` or `CITADEL_LOCAL_UPLOADS=off` turns them off, and "off"
    refuses staging, attach and the OS dialog (Q8). Rejected alternative
    (first recommendation): no setting at all.

## Findings from the code

These come from reading the current tree. The first slice that touches each
area confirms it with a test before changing it.

- **Local mode is excluded from the chooser machinery in three places.**
  `startChooserWatch` returns early for a tracked session.
  `HumanFileAttachSupported` requires `!trackTargets`. `HumanAttachStagedFile`
  refuses a tracked session. The watcher also dials `pickTargetContext` (the
  first page), while local mode follows popups and acts on the active page. So
  enabling it is new work, not a flag flip.
- **The local browser is headless (Windows) or headed under Xvfb (Linux)**,
  so a native file dialog is never visible or reachable in the viewer. On
  Linux, without interception, a GTK dialog can open on the Xvfb display
  where nobody can see it; headless Chrome cancels the chooser.
- **Upload forms are refused today.** `FormSnapshotScript` sets `bad` when
  `new FormData(form)` holds a File entry, and `add` sets it for a focused or
  targeted file input outside a form. An empty file input contributes an
  empty File, but `FormData` only includes controls that have a non-empty
  `name`. So this happens whenever the form has a named file input, which is
  the usual case for a plain HTML upload form. An unnamed one (common in
  script-driven upload widgets) does not set `bad`. `bad` leaves the target
  unresolved, `Classify` returns commit, and the pending commit is
  unverifiable. So in local mode the agent cannot type into, click or submit
  any part of a form with a named file input. That includes a click on the
  file input itself when it sits in such a form.
- **The target probe runs in the page's main world** (`InspectTarget` calls
  `runtimeEvaluate` on `targetProbeScript`). Its `env` prototype capture
  defends against named-property shadowing, such as a control named
  `elements`. It does not defend against a page that replaced the
  prototypes before the probe ran.
- **The chooser watcher clears only on a main-frame navigation.** A chooser
  whose own subframe navigates stays pending until an attach fails.
- **"upload" is a commit word** in `commitLabelWords`, so clicking a button
  labelled Upload already asks.
- **`localviewer` caps action bodies at 4096 bytes** (`handleAction`), so an
  upload needs its own route.
- **The local MCP stdio loop caps one message at 10 MiB.** `mcpBridge.run`
  uses `scanner.Buffer(..., 10*1024*1024)`, and a longer line ends the loop,
  which ends `citadel local mcp`. A 25 MiB file is about 33.5 MiB as base64,
  so agent staging must be chunked (Q2).
- **`localaudit.Record` has no file fields.** Name, size and sha256 need new
  fields.
- **`dispatchCommit` cannot replay `type`**, because typed text is never kept
  in an `actionRef`. A `type` that classifies as a commit is never
  approvable. That stays true, and it is the right answer.
- **`citadel local config <harness>` takes a harness name.** The opt-out
  setting needs a reserved word there (Q8).
- **#1584 is open.** The co-browse browser listens on a loopback CDP port. Any
  local OS user can connect to it and call `DOM.setFileInputFiles` with any
  path our user can read. That is pre-existing and independent of this design.

## Constraints

These were decided by Jason and are not reopened here.

- Local mode needs no account, sends no telemetry, makes no egress of its own,
  and needs no admin rights or Docker. `TestLocalMCPStdioMakesNoOutboundCalls`
  stays green.
- Grants and approvals come only from human-side viewer routes. No MCP tool
  can approve, accept an agent file, open the OS dialog, or attach without a
  card.
- The agent never causes Citadel to read a path the agent supplied.
- File bytes and typed text are never logged. The audit records name, size and
  sha256 only. A path the OS dialog returns is never logged, stored, shown or
  returned.
- The limits match the node path: 25 MiB per file
  (`MaxCobrowseStagedFileBytes`) and 16 files per session
  (`MaxCobrowseStagedFiles`), whichever way a file arrives.
- Windows comes first (Chrome or Edge), then macOS and Linux.

## Q1. How the human supplies a file

There are two human sources (decision 1). Both stage through the same
`StageFile` call, record `origin=user`, and need uploads to be on (Q8).

**Where the bytes live.** They go in the platform's session staging dir
(`cobrowseSession.stagingDirPath`, decision 2):

- `StageFile` creates the dir owner-only (0700, or the protected owner-only
  DACL inherited from the session base dir on Windows). It refuses a symlink,
  a junction or a group/other-accessible dir, writes create-only (`O_EXCL`),
  checks free space, and fails closed on the count.
- Every way a session ends removes it: Stop, a failed launch, the orphan
  sweep, an expired registry record, and `CleanOrphanedProfileSessions` for a
  crashed saved-profile session.

`StageFile` refuses until the session's browser is running, so a locked
saved-profile session offers no upload until the human unlocks it (or
chooses a throwaway session). The viewer hides both sources for a locked
session. For a saved-profile session, the staging dir lives inside the
decrypted working dir. Teardown already refuses to seal while removing staging
fails. An acceptance test must show that `.citadel-stage` is gone before
`Persist` seals the profile, and that the sealed blob never contains a staged
file.

**Names and duplicates.** Every name goes through
`platform.ValidateStagedName`.

- An invalid name is refused with its reason, and the card offers a rename
  field prefilled with the original name.
- A name already staged with the same sha256 is a no-op that returns the
  existing record.
- A name already staged with different content is refused ("already staged;
  remove it first").

### Source A: the viewer picker and drop zone

The viewer page already runs in the human's default browser (Edge, Chrome,
Firefox, anything) and holds the viewer bearer. It gets one
`<input type=file>` and a drop zone over the staged-file list. The page reads
the chosen `File` with the standard File API and posts the bytes to a loopback
route. The browser supplies only a base name (`File.name`), never a path.

**Upload route.** `POST /api/sessions/{id}/files?name=<percent-encoded name>`,
with a raw `application/octet-stream` body.

- It sits behind the same guard as every state change: Host, Origin
  (required), cookie, and the `X-Citadel-CSRF` bearer. All of it is checked
  before any body byte is read.
- `Content-Length` is required and must be at most
  `MaxCobrowseStagedFileBytes`, otherwise 413 with nothing read. The body is
  read through `http.MaxBytesReader` at that limit.
- One upload at a time per server (a `TryLock`; a second upload gets 409
  `busy`), so memory stays bounded at one 25 MiB buffer.
- The response is `{name, size, sha256}`. `DELETE
  /api/sessions/{id}/files/{name}` removes one staged file through a new
  platform `RemoveStagedFile` that uses the same validation as
  `resolveStagedFile`.

A drop onto the screencast is not routed into the page in v1, and
`Input.dispatchDragEvent` with files is out of scope. The drop zone only
stages.

### Source B: an OS file dialog launched by Citadel

**Who starts it.** Only the human, by pressing "Choose with the system
dialog" in the viewer. That press calls the viewer action `pick-file`, which is
behind the same guard and bearer. No MCP tool, agent argument or page event can
open the dialog. One dialog at a time per owner, and the action is refused
while one is open.

**Which process opens it.** The owner process (`citadel local mcp`, which holds
the co-browse owner lock). It owns the session and the staging dir, so the
path never has to cross a process boundary. The owner opens the dialog only
when it is in an interactive desktop session. Otherwise the viewer hides the
button and says why ("the system file dialog is not available here; use
Choose a file instead"). The checks are per OS:

- Windows: `ProcessIdToSessionId` is not 0, and the process window station is
  visible (`GetUserObjectInformationW(UOI_FLAGS)` has `WSF_VISIBLE`).
- macOS: the process is in an Aqua session (`launchctl managername` reports
  `Aqua`).
- Linux: a session D-Bus is reachable and `DISPLAY` or `WAYLAND_DISPLAY` is
  set.

**Asynchronous flow.** `pick-file` returns `{started: true}` at once. The
viewer's state poll shows `picker: "open"` while the dialog is up, then the
new staged file, or `picker: "cancelled"` or a fixed refusal reason. The HTTP
request never waits on a person.

**How the path is used.** The dialog code returns the path to one function,
`readPickedFile`, in the same goroutine. Nothing else sees it. That function:

1. opens the path once and does every later check on that handle, never
   reopening the path;
2. requires a regular file: no directory, device, FIFO, socket or (on Windows)
   pipe or console handle;
3. refuses a file on a network filesystem (below), so Citadel never transfers
   bytes from a remote host;
4. reads at most `MaxCobrowseStagedFileBytes + 1` bytes through the handle, and
   refuses an over-limit file without staging anything;
5. hashes the bytes, takes the base name for staging (with the rename offer if
   it is invalid), and calls `StageFile`;
6. drops the path. Logs, the audit, errors and responses carry name, size and
   sha256 only, and errors use fixed reasons.

`readPickedFile` takes the path from a `filePicker` interface, so CI tests it
with temp files (a directory, a symlink to a large file, a FIFO on Unix, a
file over the limit, a file that grows after open) and a fake remote check. The
dialog itself is native acceptance on a real desktop, post-merge, per OS.

**Consent.** Picking a file in the OS dialog is consent to stage it, the same
as Source A. It is not consent to send it: every attach still needs the
human's press or an approval (Q3).

**Per OS** (Windows first):

- **Windows.** `IFileOpenDialog` on a dedicated goroutine with
  `runtime.LockOSThread`, COM initialized apartment-threaded
  (`CoInitializeEx(COINIT_APARTMENTTHREADED)`). Options are
  `FOS_FILEMUSTEXIST | FOS_FORCEFILESYSTEM | FOS_NOCHANGEDIR |
  FOS_DONTADDTORECENT`, with the title "Choose a file for Citadel co-browse".
  The dialog is owned by a hidden topmost window, so it is not left behind
  the browser. Windows' foreground lock may still keep it from taking focus,
  in which case it flashes in the taskbar. The viewer says "A Windows file
  dialog opened; it may be behind this window" while `picker` is `open`. COM
  goes through `go-ole`, which is already in the module graph as an indirect
  dependency; the alternative is a small hand-written vtable. The read uses
  `CreateFileW` with `FILE_SHARE_READ`, then `GetFileType` (`FILE_TYPE_DISK`)
  and `GetFileInformationByHandle` (not a directory). It resolves
  `GetFinalPathNameByHandleW` and refuses a UNC path or a volume whose
  `GetDriveTypeW` is `DRIVE_REMOTE`. When the human browses the dialog's
  Network node, that SMB traffic is the Windows shell acting on the human's
  navigation, not a Citadel connection. Citadel still never reads a remote
  file.
- **macOS.** A child `osascript -e 'POSIX path of (choose file with prompt
  "Choose a file for Citadel co-browse")'`, with the path read from its
  stdout. `choose file` needs no Automation permission. Cancel (exit 1, error
  -128) is `cancelled`. The read refuses a file whose `statfs` type is
  `smbfs`, `nfs`, `afpfs` or `webdav`.
- **Linux.** The `org.freedesktop.portal.FileChooser.OpenFile` call through
  `godbus` (already a direct dependency), with the `file://` URI read from
  the `Response` signal. If no portal answers, the fallback is `zenity
  --file-selection`, then `kdialog --getopenfilename`, and the button is
  hidden if none works. The read refuses `statfs` magic for NFS, CIFS/SMB2
  and FUSE.

## Q2. Who may stage

**The human**, through either source in Q1.

**The agent, with acceptance (decision 3).** The tool is
`local_cobrowse_stage_file {session_id, name, size, sha256, offset,
data_base64, goal}`.

- Bytes arrive in chunks of at most 4 MiB decoded (about 5.4 MiB as base64),
  because one stdio message is capped at 10 MiB (Findings). The first call
  has `offset` 0 and declares `name`, `size` (at most 25 MiB) and `sha256`.
  Each later call must continue at the next offset, with the same name, size
  and sha256. A gap, overlap, mismatch or over-size chunk aborts the upload.
  Alternative (rejected): raise the local stdio cap to about 40 MiB and take
  one call. That pins a large buffer per message, and many agent apps cannot
  emit a tool argument that size.
- Each chunk is a write for the per-minute budget. There is one agent upload
  in progress per session, and one waiting for acceptance.
- The bytes stay in the owner process's memory. Nothing touches disk before
  acceptance. An upload idle for 2 minutes is dropped. Once complete, the
  sha256 must match the declared one, the name must pass
  `ValidateStagedName`, and the stage-time checks (count, free space,
  duplicate) are pre-run so the card never offers what cannot be staged.
- The tool returns `{state: "receiving", received}` until the last chunk, then
  `{state: "awaiting_user"}`. The agent waits with
  `local_cobrowse_wait until=file`.
- The viewer shows the acceptance card (Q5): name, size, the full sha256, a
  type detected from the first 512 bytes (`http.DetectContentType`, shown as
  "detected", not trusted) and "Provided by the agent". **Accept** stages it
  with `origin=agent` through `StageFile`. **Reject** drops the bytes, and
  that sha256 is refused for the rest of the session. An unanswered card
  expires after `PendingCommitTTL` (10 minutes).
- Uploads off (Q8) refuses the first chunk.

Accepting is consent to stage, not to send. Every attach of an agent file
still asks (Q3), and every card shows the file's origin.

**What the agent sees of staged files.** `local_cobrowse_status` lists
`staged_files` as name, size and origin. The agent also sees sha256 for files
it provided, since it already has them. It never sees a file's content or
where on disk a human-picked file came from.

## Q3. Who may attach, and how it fits the commit model

**Attaching is the commit, not the later submit.** Once `DOM.setFileInputFiles`
lands, the page's `change` handler can read the file and `fetch` it to any
public origin. `EgressGuard` only stops non-public addresses, and a form
`action` says nothing about where script sends the file. So the consent
question is always "may this page have this file", and every card says so
plainly: "The page on <site> receives this file and can send it anywhere."

### Human attach

`Service.HumanAttachFile(ctx, id, chooserID, stagedName)` (decision 4).

- It is reached only from the viewer `attach-file` action, never from an MCP
  tool.
- It runs under the session's `wmu`. That serializes it against every agent
  write and against `ApproveCommit`, so it works whether the agent or the
  human is driving.
- The human's press is in-the-moment consent, like `HumanNavigate`. No domain
  grant is needed, and the grant is not changed.
- It does not mark the session identity-bearing: attaching a file is not a
  sign-in.
- A human attach to the chooser of a pending agent attach request resolves
  that request as `superseded`.

### Agent attach

`local_cobrowse_set_file_input {session_id, staged_name, selector?, goal}`
(decisions 4 and 5).

- It targets the pending chooser by default, or a file input matched by
  `selector` in the main frame, as on the node.
- It is always a commit, never covered by a grant, and it runs through
  `writeAction`. The probe captures the target. The pending commit's
  `actionRef` carries the staged name, the staged sha256, the chooser id or
  selector, the frame id, and the frame's browser-side URL (from
  `Page.getFrameTree`, so the page cannot influence it).
- The fingerprint covers all of these, plus the page URL. The card shows the
  file (name, size, origin, first 12 hex of sha256), the frame's site and a
  screenshot.
- `ApproveCommit` re-resolves the staged file and refuses if the sha256
  differs. It re-checks uploads on, then dispatches through `SetFileInput`
  with the captured frame URL as `expected_url`. The agent never supplies
  `expected_url` in local mode; the service does.
- An approval is good for that one attach only. A denial is sticky for that
  fingerprint, as today.

### Shared fences

All three entries share `fillFileInput`. Only the fences differ:

| Binding | Node agent (`SetFileInput`) | Node human (#1728) | Local, human press or approved agent attach (new) |
|---|---|---|---|
| Who may run it | agent, not while handed off | viewer with control | viewer route or `ApproveCommit`, under `wmu` |
| Page | pinned action page | first page | the chooser's target id (`ExpectTarget`), still active |
| Chooser | backend node id matches | same pointer, same node id | same chooser id (a generation counter), same node id |
| Frame | `expected_url` from the caller | URL captured at open | URL and loaderId captured at open (or at the agent's request) |
| Driver | `driverFence` | `humanControlFence` | driver unchanged since the attach began |
| Uploads | Files permission | Files permission | uploads on (Q8), re-read before dispatch |
| Requester | none | viewer ctx not done | HTTP request ctx not done |

### A pending chooser does not block the agent (decision 6)

The agent keeps acting while a chooser is pending. What it sees, in
`local_cobrowse_status`:

```
"file_chooser": {
  "pending": true,
  "chooser_id": 7,
  "multiple": false,
  "field_label": "Upload your ID",
  "frame_site": "example-portal.com",
  "opened_at": "2026-11-17T15:02:11Z",
  "agent_acted_since": true
},
"last_file_event": {"kind": "attached", "by": "user", "staged_name": "utility-bill.pdf", "at": "..."}
```

`local_cobrowse_wait until=file` returns on any change to `file_chooser`,
`staged_files`, a pending acceptance, or `last_file_event`. Kinds are
`attached`, `dismissed`, `cleared`, `refused`, `accepted` and `rejected`, and
an attach records whether the user or the agent did it.

**How stale choosers are cleared.** A pending chooser is cleared, and the
agent and viewer see `last_file_event.kind = "cleared"` with a fixed reason,
when any of these happens:

- the main frame navigates, or the chooser's own frame navigates (new: today
  only the main frame clears it);
- the active target switches or closes;
- a newer chooser opens, replacing it;
- after an agent write that dispatched, the service re-resolves the chooser's
  backend node in an isolated world (one `DOM.resolveNode` plus
  `checkFileInput`), and the node is gone or is no longer a file input. An
  SPA that closes its modal is cleared this way;
- the same check runs before the card's Attach button is enabled;
- the human presses Not now;
- 10 minutes pass (`PendingCommitTTL`);
- the session stops;
- any attach is refused with `page_changed`, which clears it (as
  `clearChooserIf` does on the node).

While `agent_acted_since` is true, the card says "The agent has used the page
since this request opened."

### Clicking the file input, and submitting with a file

**Clicking the file input.** A click whose target is an `input[type=file]`, or
a `label` whose control is one, only opens a chooser, and someone must still
attach a file. So it is classified as fill (S1). A button named Upload stays a
commit, because on many sites it sends files already attached.

**Submitting a form with a file (decision 7, S1).** Change `FormSnapshotScript`
so that a File entry no longer sets `bad`. It encodes as a tuple
`[name, "file", fileName, size, type, lastModified]`.

At the design pass the probe ran in the main world (see Findings), where a
page could replace `File.prototype` getters before it ran. S1 uses the cleaner
end state: `InspectTarget` runs the complete probe in a browser-created
isolated world (`Page.createIsolatedWorld`). Target resolution, focused state,
the native submission set and native File/Blob getters use that same world's
DOM observation, rather than merging two observations across worlds. A missing
world, failed read or changed driver refuses the probe without falling back to
the main world. The session reuses the context only for the same target, frame
and loader, retaining at most one context reference. A lost context is invalidated
and the current probe refuses; the next probe reads the current frame tree and
recreates the world, without retrying any action. Chooser classification comes
from a pristine native file-input type or a label's native associated control,
not attributes on the label. A label for a non-file control is classified from
that control's tag, type, name and form; misleading label text cannot hide a
submit or Delete button. An uninspectable associated custom control is refused.
Interactive descendants stay their own targets, and an actionable chooser
still passes through the commit-word checks. While the human drives, agent
views omit picked-file metadata; the human viewer retains its approval evidence.
The browser's synthetic empty File timestamp is normalized to
zero, so an unselected file input has a stable digest. A snapshot with more
than 128 attached entries is unverifiable rather than producing an unbounded
commit card. Filename display strips Unicode format and control characters and
truncates the middle to retain the final extension. An extension too long for
the bounded display is explicitly unavailable and makes approval unverifiable.
No file bytes or local paths are read. Then:

- fills in that form are ordinary fills under the grant;
- the form's Submit is an ordinary commit whose content hash changes if the
  files change after approval;
- the commit card lists the attached file names and sizes, which the service
  matches to staged records.

The file content is not hashed in the page. The page already holds the bytes
and could send any of them, so hashing adds nothing at submit time. The trust
decision was the attach.

## Q4. Consent for type with selector or clear, and key chords

Decision 9 accepted this section as written.

PR #1731's allowlist is final and is not re-derived here. The allowed chords
are:

- Shift with a printable key, an arrow, Home, End, PageUp, PageDown, Tab,
  Enter or Backspace;
- Ctrl or Meta with a;
- Ctrl with an arrow, Home, End, Backspace or Delete.

Anything with Alt, undo and redo, and clipboard chords are refused. Local mode
calls `platform.CheckKeyChord` before any gate, so a refused chord costs no
budget and is audited as a refusal.

**What the probe describes.** Today `TargetProbe{Kind: "type"}` describes the
focused element. With a selector it must describe the selector's element
instead, and the classifier, the fingerprint and the pre-dispatch re-probe all
use that description. `actionRef` gains `Clear` and `Modifiers`, and the
fingerprint struct's version goes to 3. So an approved Enter cannot be
redeemed as Shift+Enter, and a plain type cannot be redeemed as a clear.

**What the human approves.** A fill has no per-action card. The domain grant
covers it, and the grant card's wording changes to: "The agent may click, type,
replace what is in fields and press keys on <site>. Sending, posting,
submitting, paying, deleting and attaching files always ask." The Activity
panel records each edit as, for example, "Replaced the contents of 'Email'
(12 characters)". It shows the length, the element label and `cleared`, never
the text.

**Classification:**

| Action | Target | Class |
|---|---|---|
| `type` (no selector, no clear) | focused field | fill (unchanged) |
| `type` with selector and/or clear | resolved editable field | fill |
| `type` with selector | not editable, hidden, password or one-time-code | refused before dispatch, nothing sent |
| `type`, any form | unresolved | commit, never approvable (text is not kept): "do it yourself" |
| Ctrl/Meta+A | anywhere | fill |
| Shift+Tab, Shift+arrows/Home/End/PageUp/PageDown, Ctrl+arrows/Home/End | anywhere | fill |
| Shift+printable, Shift+Backspace, Ctrl+Backspace, Ctrl+Delete | text field | fill |
| the same chords | outside a text field | commit, like the bare key (page shortcuts) |
| Shift+Enter | anywhere | same as Enter: fill only in a search entry, a named control is judged by its label, anything else asks |

"Hidden" means:

- the element's box is empty after focus;
- or it lies wholly outside the viewport;
- or its computed `visibility` is hidden;
- or its opacity is below 0.1.

That stops an agent from writing into a field the human cannot see and then
asking them to approve a submit whose screenshot does not show it. Typing into
a password or one-time-code field through selector stays refused, because the
local tool's own guidance is to hand off. Plain `type` into a focused password
field keeps its current behavior; changing that is a separate decision.

`type` keeps the newline refusal in every form. A clear on a composer is a
fill: it changes local state only, and the next submit's content hash
captures the result.

**Residual.** The service's re-probe and `TypeWith`'s own resolve are separate
calls, so a page could swap which element matches the selector between them.
`TypeWith` still refuses a non-editable target and verifies its own binding
(`verifyBound`) before the destructive event. The swap can only move a fill to
another visible, editable field that matches the same selector. This matches
the existing residual for a selector click. An `Expect` field on
`TypeOptions` (tag, type, label) could close it later.

## Q5. Viewer UX

**The file card** appears when a chooser is pending on the active page. It
raises attention the way approval does, and the tab title shows "waiting for
you". It does not freeze the agent (decision 6).

```
+--------------------------------------------------------------+
| The page on example-portal.com is asking for a file          |
| Field: "Upload your ID"  (accepts one file)                  |
| The agent has used the page since this request opened.       |
|                                                              |
|  The agent proposes: invoice-2026-11.pdf  182 KB  (agent)    |
|   [ Approve the agent's attach ]   [ Deny ]                  |
|                                                              |
|  Or attach a staged file yourself:                           |
|   ( ) passport-scan.pdf      2.1 MB   you          [Remove]  |
|   (o) utility-bill.pdf       640 KB   you          [Remove]  |
|                                                              |
|  [ Choose a file ]  [ Choose with the system dialog ]        |
|  or drop a file here                                         |
|                                                              |
|  The page receives the file you attach and can send it       |
|  anywhere. Attach only if you expected this request.         |
|                                                              |
|  [ Attach utility-bill.pdf ]   [ Not now ]                   |
+--------------------------------------------------------------+
```

- The "agent proposes" row appears only while an agent attach request is
  pending for this chooser. It shows the commit card's screenshot on expand.
- An agent attach aimed at a selector, with no chooser pending, uses the
  standard commit card: screenshot with the target highlighted, then
  "Attach passport-scan.pdf (2.1 MB, from you, sha256 3f9a1c...) to the file
  field on example-portal.com", with Approve or Deny.
- **Attach** calls `attach-file`. On success the card closes and the status
  line says "Attached utility-bill.pdf to example-portal.com". A refusal shows
  the fixed reason (page changed, chooser closed, file removed, uploads off).
  The card stays only if the chooser is still pending.
- **Not now** calls `dismiss-file`. It clears the chooser, and the agent sees
  `dismissed`.

**The agent file acceptance card:**

```
+--------------------------------------------------------------+
| The agent wants to add a file to this session                |
|   Name:     invoice-2026-11.pdf                              |
|   Size:     182 KB        Type: PDF document (detected)      |
|   SHA-256:  3f9a1c0b...  [show all]                          |
| Provided by the agent. Nothing is saved until you accept.    |
| Accepting does not send it anywhere; attaching it will ask.  |
|   [ Accept ]   [ Reject ]                                    |
+--------------------------------------------------------------+
```

**Other behavior.**

- **The staged list** also shows under the session controls when no chooser is
  pending. It shows name, size, origin and a count ("2 of 16 files"), with
  Remove buttons. The full sha256 is in a details row.
- **The system dialog.** While it is open, the button reads "Dialog open",
  with the line "A file dialog opened; it may be behind this window".
- **Uploads off.** The picker, drop zone and dialog button are replaced by
  "Uploads are turned off on this computer (`citadel local config uploads
  on` turns them back on)". Staged files can still be removed.
- **Hand back.** When the human has control, the card works the same way.
  Hand back is unchanged.
- **Rendering.** File names may contain anything. They are rendered with
  `textContent` only, never markup, and truncated in the middle to 60
  characters.

## Q6. Threats

**A hostile page.**

- *Fake choosers.* A page can draw its own "choose a file" UI inside the
  screencast. The real card lives outside the picture, in the viewer's own
  chrome, and is raised only by the browser's `Page.fileChooserOpened` event.
  The human only ever picks files in the viewer's picker or the OS dialog
  Citadel opens from the viewer. A page can also call `input.click()` from
  any handler the agent's click triggers. The card then appears unasked,
  which is why it names the site, the field label and the warning line.
- *Prototype spoofing.* Resolution and `checkFileInput` run in an isolated
  world (`Page.createIsolatedWorld`). The frame URL comes from
  `Page.getFrameTree`, never from page script. File entries in the form digest
  are read in an isolated world (Q3).
- *Navigating mid-attach.* The #1728/#1731 lesson applies: every awaited CDP
  read is followed by a re-check of every binding, with nothing awaited
  between the last check and `DOM.setFileInputFiles`. The bindings are:
  - the session is alive, on the same port and browser id;
  - the chooser id is the same;
  - the target is still active;
  - the frame URL and loaderId are unchanged;
  - the driver is unchanged;
  - uploads are on;
  - the request context is not done;
  - `wmu` is held.

  Because agent writes are not blocked (decision 6), these fences carry the
  weight, so a slice adds a fake-CDP test for each binding failing after each
  awaited read. A same-URL reload changes the loaderId, which the URL check
  alone would miss.
- *Swapping the input.* A replaced element is a different backend node and
  fails the chooser match. After an attach, a page can swap `input.files`
  with its own File. It only fools itself: it already had the bytes.
- *Cross-site frames.* Refused in v1 (decision 8). The card would otherwise
  name the tab's site while the file goes to the frame's.

**A hostile or confused agent.**

- *Exfiltration by upload.* Agent bytes need the human's acceptance, and every
  agent attach needs a per-upload approval naming the receiving site, the
  file and its origin. A human attach needs the human's press. None of this
  stops a human who is socially engineered into attaching a sensitive file,
  and the warning line is the mitigation. After an attach the agent can read
  whatever the site renders of the file. That is ordinary page reading of a
  site the human chose to give the file to.
- *Racing the human (decision 6).* While a chooser is pending, the agent can
  navigate, open another chooser or change the field. Each of those clears or
  replaces the chooser, or fails a fence, so the human's attach goes nowhere
  else. It can never attach to a field other than the one its card showed.
- *Disguised agent files.* An agent file named like a personal document still
  carries "Provided by the agent" on every card. Its type is shown as
  detected from the bytes, and a rejected sha256 cannot be offered again in
  the session.
- *A path from the agent.* No tool takes a path. Staged names pass
  `ValidateStagedName` and resolve only inside the session's staging dir
  (`resolveStagedFile`: regular file, no symlink, the real dir equals this
  session's). The OS dialog path comes only from the OS dialog, which only
  the viewer can open.
- *Huge or many files.* There are 25 MiB and 16-file limits on every source,
  checked server side, and the size is refused before the body is read.
  Viewer uploads are serialized. One agent upload is in progress and one is
  awaiting acceptance per session, in memory. Chunks count against the write
  budget, free space is checked, and the count fails closed. A chunk is at
  most 4 MiB, so the 10 MiB stdio cap is never reached.
- *Prompt spam.* There is one acceptance card and one pending commit at a
  time, cards expire after 10 minutes, and rejected hashes are sticky.
- *Chord abuse.* `CheckKeyChord` refuses clipboard chords, so the agent cannot
  paste or copy through the browser. Shift+Enter and editing chords outside a
  field ask (Q4).
- *Writing into fields the human cannot see.* Selector targets must be visible
  (Q4).
- *Turning uploads back on.* Only the CLI and the environment change the
  setting (Q8). No MCP tool or viewer route does. An agent that can run shell
  commands as the user could run the CLI itself. That agent already has the
  user's files, and is outside this design's boundary.

**The OS dialog.**

- *Timing.* Only the human's press opens it, so neither a page nor the agent
  can make a dialog appear at a moment of its choosing.
- *Remote files.* A file on a network filesystem is refused, so Citadel never
  pulls bytes from a remote host (Q1).
- *Special files.* A directory, device, FIFO, socket or pipe is refused on the
  opened handle. The read is bounded, so a file that grows after open cannot
  exceed the limit.
- *Path disclosure.* The path stays inside `readPickedFile` and is never
  logged, stored or returned.

**Other local users on the same machine.**

- *The loopback viewer port.* Every OS user can reach 127.0.0.1. The cookie
  alone unlocks nothing, because browsers send it to every 127.0.0.1 port.
  The bearer exists only in the `/open` page's sessionStorage, minted from a
  single-use code in an owner-only launch file. The upload, attach, accept and
  pick-file routes check both before doing anything. Without the bearer,
  another user gets a 403 and nothing is buffered beyond the request headers.
- *File permissions.* Unix: the session base dir must be owned by our uid and
  not group/other-writable. The staging dir is 0700, files are 0600, and
  `existingStagingDir` refuses anything group/other-accessible. Windows: the
  base dir requires a protected owner-only DACL
  (`validateCobrowseBaseDirPerms`), and the staging dir and files inherit it.
  A windows-latest test reads the DACL of the staging dir and of a staged
  file, and requires exactly what `validateCobrowseBaseDirPerms` accepts:
  every allow ACE names the current user and nothing else. The settings file
  (Q8) is owner-only the same way.
- *The debug port (#1584).* Another user can drive the browser over its
  loopback CDP port. That includes `DOM.setFileInputFiles` with any file our
  user can read. This exists today without upload. #1584 closes it, and it
  must land before the quickstart advertises uploads (decision 10).

**Audit.** `localaudit.Record` gains `file_name`, `file_size`, `file_sha256`
and `file_origin`.

- Human events, Class H with actor user: `stage_file`, `pick_file`,
  `remove_file`, `file_attach`, `file_dismiss`, `accept_file` and
  `reject_file`.
- Agent events, actor agent: `stage_file_offer` (one record when the upload
  completes, not one per chunk), plus the `set_file_input` request, which
  reuses the commit events.
- `file_cleared` is recorded with actor citadel.
- URLs go through the existing origin-plus-path-hash reduction.
- Error text is a fixed reason, never a path.
- `TestLocalAuditRecordsEveryCallWithoutSecrets` extends with sentinels in
  the file bytes, in the session dir path, and in an OS-dialog path, and none
  may appear anywhere in the log.

## Q7. Slices

In dependency order. Each slice is sized for one agent. "Opus review" marks
browser-control or security slices. Real-browser tests run on Linux and on
windows-latest with both Chrome and Edge.

**S1. File-aware form digest and file-input clicks.** Start first.
- Area: `internal/platform/cobrowse_target.go`, `internal/localcobrowse/commit.go`.
  Opus review (security).
- Scope:
  - file entries encoded in the digest, read in an isolated world (Q3);
  - a click on a file input or its label classified as fill;
  - the commit card lists the attached files;
  - `RuleVersion` bumped.
- Acceptance:
  - first, a real-browser test that pins today's behavior: a text fill
    inside a form with a named file input is refused, and one inside a form
    with an unnamed file input is not. Then the named case flips;
  - `formsnapshot_test` cases: an empty named file input, a file set through
    CDP, and files swapped after approval (the fingerprint changes);
  - a real-browser test in which the page replaces `File.prototype` getters
    in the main world before the probe runs, and the digest still reflects
    the real file name and size;
  - commit browser test: fill and submit an upload form whose file input
    was set through CDP, with the card listing the file.
- Order: no dependencies.

**S2. Uploads opt-out setting.**
- Area: `cmd` (`local_config.go`, `local_doctor.go`, `mcp_local_cobrowse.go`),
  `internal/localcobrowse`. Opus review (security, small).
- Scope: everything in Q8.
- Acceptance:
  - each source of "off" refuses through the shared gate;
  - an unreadable or corrupt file reads as off;
  - an edit takes effect without a restart;
  - `config uploads` never touches harness files;
  - `local doctor` shows the state.
- Order: no dependencies. S6, S7, S8 and S9 call its gate.

**S3. Chooser watch for local sessions.**
- Area: `internal/platform`. Opus review (browser-control).
- Scope:
  - lift the `s.tracking` early return;
  - attach the watcher to the active target, and redial on a target switch
    or close;
  - key the pending chooser by target id, with a generation counter as its
    chooser id;
  - record the frame URL and loaderId;
  - clear it on navigation of the main frame or of the chooser's own frame,
    on a target change and on session stop;
  - record a subframe chooser whose frame site differs from the top page's
    with `cross_site: true`, so it can be refused.
- Acceptance:
  - fake-CDP unit tests for each transition;
  - real browser: a chooser opened on the active page is recorded, a tab
    switch clears it, a chooser in a popup after the switch is recorded, a
    reload clears it, and a subframe navigation clears that frame's chooser.
- Order: no dependencies.

**S4. Owner attach on the platform.**
- Area: `internal/platform`. Opus review (security).
- Scope:
  - an owner-attach entry that shares `fillFileInput` and uses the local
    fence set (Q3 table), for both the human press and an approved agent
    attach;
  - a `RevalidateChooser` call (isolated-world `DOM.resolveNode` plus
    `checkFileInput`);
  - `RemoveStagedFile`;
  - the local runtime wires `SetFilesAllowed` to the S2 gate. Local mode
    attaches only through the viewer HTTP action and `ApproveCommit`: the
    stream's `file_attach` input stays off for a tracked manager, and the
    existing fail-closed test is replaced by one that pins exactly that.
- Acceptance:
  - fake-CDP tests in which each fence fails in turn after each awaited
    read, and nothing is dispatched;
  - a cross-site chooser is refused;
  - a wrong chooser id is refused;
  - uploads turned off between the start and the dispatch are refused;
  - real browser: an attach after a same-URL reload is refused,
    `RevalidateChooser` clears a chooser whose input was removed, and an
    attach to a live chooser delivers the bytes and the page sees the
    staged name.
- Order: after S3; uses the S2 gate.

**S5. Service: human staging and attach, status, wait, stale clearing, audit.**
- Area: `internal/localcobrowse`, `internal/localaudit`, `cmd/mcp_local_cobrowse.go`.
  Opus review (security).
- Scope:
  - `HumanStageFile`, `HumanRemoveFile`, `HumanAttachFile` and
    `HumanDismissFile`, all under `wmu`;
  - origin tracking per staged name;
  - the `file_chooser`, `staged_files` and `last_file_event` status fields;
  - `wait until=file`;
  - re-validate the chooser after each dispatched agent write and before
    enabling Attach, plus the 10-minute expiry;
  - the new audit fields;
  - the `Browser` interface and `fakebrowser` extended.
- Acceptance:
  - unit tests on the fake browser;
  - an agent write while a chooser is pending is not blocked;
  - every stale-clearing trigger in Q3 produces `cleared` with its reason;
  - `TestNoLocalMCPToolCanApprove` extended so that no tool reaches stage,
    remove, attach, dismiss, accept or pick-file;
  - the audit sentinel test (Q6);
  - `TestLocalMCPStdioMakesNoOutboundCalls` stays green.
- Order: after S4.

**S6. Viewer upload routes and UI, plus end-to-end acceptance.**
- Area: `internal/localviewer`, `cmd`. Opus review (security).
- Scope:
  - the upload and delete routes (Q1, Source A);
  - the `attach-file` and `dismiss-file` actions;
  - the `app.js` file card, staged list and drop zone;
  - the uploads-off state;
  - local runtime wiring.
- Acceptance:
  - guard tests: no cookie, no bearer, a foreign Origin or a missing Origin
    each give 403 with the body unread; over-limit Content-Length gives 413;
    a 17th file is refused; a bad name is refused; a duplicate is handled;
    a second concurrent upload gets 409; uploads off is refused;
  - real browser end to end: the viewer uploads, the page opens a chooser,
    the card appears, Attach runs, and the test page receives bytes whose
    sha256 matches;
  - staging is gone after `local_cobrowse_stop` and after `HumanStop`;
  - a saved-profile session's sealed blob never contains a staged file;
  - windows-latest DACL check on the staging dir and a staged file;
  - a non-ASCII file name round-trips on Windows.
- Order: after S5.

**S7. Agent `local_cobrowse_stage_file` and the acceptance card.**
- Area: `internal/localcobrowse`, `cmd/mcp_local_cobrowse.go`,
  `internal/localviewer`. Opus review (security).
- Scope:
  - the chunked tool (Q2), held in memory, with abort and idle-drop rules;
  - the `accept-file` and `reject-file` viewer actions and the card;
  - sticky rejection by sha256;
  - `origin=agent` on every later card.
- Acceptance:
  - out-of-order, overlapping, over-size, mismatched-hash and
    over-25-MiB uploads are refused;
  - nothing is on disk before Accept (the staging dir is unchanged);
  - a rejected sha256 cannot be offered again;
  - no MCP tool can accept;
  - a full 25 MiB file staged in 4 MiB chunks keeps every stdio line under
    the 10 MiB cap (the test drives the real `mcpBridge` loop);
  - uploads off refuses the first chunk.
- Order: after S6.

**S8. Agent `local_cobrowse_set_file_input`, always asking.**
- Area: `internal/localcobrowse`, `cmd`, `internal/localviewer` (the
  "agent proposes" row). Opus review (security).
- Scope:
  - always-ask commit, with an `actionRef` and fingerprint over the staged
    sha256, the target, the chooser or selector, and the frame URL;
  - `ApproveCommit` dispatch through the S4 owner attach, or `SetFileInput`
    for a selector, with the service-supplied `expected_url`;
  - a human attach to the same chooser supersedes the request.
- Acceptance:
  - the grant never covers it;
  - a re-staged file with a different sha256 makes the approval "changed";
  - a navigation between approval and dispatch makes it "changed";
  - a denial is sticky;
  - uploads off between approval and dispatch refuses;
  - real browser: approve and deliver, both for a human-staged file and for
    an agent-staged one.
- Order: after S1, S5 and S6. S7 is not required, but it is needed to test
  agent-origin files.

**S9. OS file dialog on Windows.**
- Area: `internal/platform` (windows-only picker), `internal/localcobrowse`,
  `internal/localviewer`. Opus review (security).
- Scope:
  - `filePicker` interface;
  - shared `readPickedFile` (all OSes);
  - interactive-session detection;
  - `IFileOpenDialog` on a locked COM thread with a hidden topmost owner;
  - UNC and `DRIVE_REMOTE` refusal;
  - the `pick-file` action, the `picker` state and the button.
- Acceptance:
  - CI: `readPickedFile` with an injected picker refuses a directory, an
    over-limit file and a remote path (faked), handles a file that grows
    after open, and never logs the path (sentinel);
  - no MCP tool reaches `pick-file`;
  - uploads off refuses;
  - a non-interactive session hides the button;
  - native acceptance post-merge on a real Windows 11 desktop: the dialog
    opens from the viewer, the chosen file is staged, cancel stages nothing,
    and a file on a mapped network drive is refused.
- Order: after S6 and S2.

**S10. OS file dialog on macOS and Linux.**
- Area: `internal/platform`. Opus review (security).
- Scope:
  - the `osascript choose file` picker and its Aqua check;
  - the xdg-desktop-portal picker through `godbus`, with the `zenity` and
    `kdialog` fallbacks;
  - `statfs` network refusals.
- Acceptance:
  - CI on Linux: the portal flow against a fake D-Bus portal, the fallback
    order, and `statfs` refusal through an injected check;
  - native acceptance post-merge on a macOS desktop and a GNOME desktop.
- Order: after S9 (shares `readPickedFile`).

**S11. Type with selector and clear, and key modifiers, in local mode.**
- Area: `internal/localcobrowse`, `cmd/mcp_local_cobrowse.go`. Opus review
  (browser-control).
- Scope:
  - `local_cobrowse_type` gains `selector` and `clear`;
  - `local_cobrowse_key` gains `modifiers`, in the same shape aceteam#13782
    settles on for the platform tool (the CDP bitmask);
  - `Browser.TypeWith` and `Browser.KeyWith`;
  - the probe describes the selector's target, plus the visibility and
    password checks;
  - the Q4 classification table;
  - fingerprint v3;
  - `dispatchCommit` passes modifiers;
  - the audit records the modifier mask, `cleared` and the length;
  - grant card wording.
- Acceptance:
  - unit tests for each table row;
  - an approved Enter cannot be redeemed as Shift+Enter;
  - real browser: selector plus clear replaces an input, a textarea and a
    contenteditable;
  - Ctrl+A then type replaces;
  - Shift+Enter in a textarea asks;
  - Ctrl+Backspace outside a field asks;
  - a hidden selector target is refused;
  - a password target through selector is refused.
- Order: starts after PR #1731 merges; independent of S1 to S10.

**S12. Docs.**
- Area: `docs/local-browse-quickstart.md`, `local doctor` text. Sonnet.
- Scope:
  - the upload flows for Windows users (picker, system dialog, agent files);
  - the warning line;
  - the limits;
  - the opt-out.
- Order: the quickstart only advertises uploads once #1584 has landed
  (decision 10).

Parallel starts: S1, S2, S3 and (once #1731 merges) S11 have no dependencies
on each other. The main chain is S3, S4, S5, S6, then S7 and S8, then S9 and
S10.

## Q8. The uploads opt-out (decision 11)

**Default.** Uploads are on.

**Two switches; either one turns uploads off.**

- The environment variable `CITADEL_LOCAL_UPLOADS`. A falsey value (`0`,
  `false`, `no`, `off`) turns uploads off. It is read once, when the owner
  process starts, because a process's environment does not change. Any other
  non-empty unrecognized value is ignored with a warning in `local doctor`;
  `1`, `true`, `yes` and `on` are recognized no-op values. It never turns
  on what the file turned off.
- The setting file `local-settings/settings.json` under `localStateDir`, holding
  `{"uploads": "on"|"off"}`. It is owner-only (written with
  `platform.EnsurePrivateDir` rules and an atomic replace) and read **live**:
  every gate check stats it and re-reads it when the size or mtime changes.
  So `citadel local config uploads off` takes effect on the next check
  without restarting the agent app. A missing file means on. A file that
  exists but cannot be read or parsed means **off**, and `local doctor` and
  the viewer say why.
  The dedicated subdirectory has its own protected owner-only permissions;
  the legacy state container may retain its inherited Windows ACL, but may not
  grant an unprivileged other user write access. An untrusted existing
  directory fails closed even when the file is missing. The wired owner
  defaults on when no file exists; an unwired service policy always denies.

**Command.** `citadel local config uploads on|off|status`. `uploads` is a
reserved word in `local config` and can never be a harness name. It writes
only the setting file and never touches harness configs. There is no MCP tool
or viewer route that changes the setting. The viewer only shows it.

**What "off" refuses.** One gate, `Service.uploadsAllowed()`, is used by:

- viewer upload (Source A), before the body is read;
- `pick-file` (Source B), before a dialog opens, and again before staging
  the picked bytes;
- `local_cobrowse_stage_file`, on the first chunk and again on Accept;
- human attach and agent attach requests, and again in the dispatch fence
  right before `DOM.setFileInputFiles` (through `SetFilesAllowed`, which the
  platform already re-reads at dispatch);
- `ApproveCommit` for an attach.

Files staged before the switch turned off stay listed and can be removed, but
cannot be attached. They are removed with the session as usual.

## Not covered

- Uploads through drag-and-drop onto the page (`Input.dispatchDragEvent`).
- More than one file per attach for a `multiple` input. v1 attaches one file
  per press or approval, as the node path does. Multi-select is a small
  follow-up.
- Choosers on background pages. Only the active page is watched, which
  matches what the human sees and where actions go.
- A cloud-viewer equivalent. The node and cloud path is #1723 and
  aceteam#13831.
