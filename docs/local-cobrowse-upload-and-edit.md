# Local co-browse: file upload, and field editing parity

Status: design only. No code ships with this document.

Refs #1729 (local-mode file upload) and #1730 (type with selector and clear,
key modifiers). Builds on #1721 / PR #1722 (staging, `set_file_input`, chooser
interception), #1723 / PR #1728 (human attach on the node path) and PR #1731
(`TypeWith`, `KeyWith`, `CheckKeyChord`, still in review). The platform side of
the edit contract is aceteam#13782.

## Open decisions for Jason

Each item gives the recommendation first, then the alternatives.

1. **How the human supplies a file.** Recommendation: a file picker and a drop
   zone in the local viewer page. The human's own default browser reads the
   file and uploads the bytes to the loopback viewer. Citadel never opens a
   dialog and never reads a path. Alternatives: an OS file dialog launched by
   Citadel; both.
2. **Where staged bytes live.** Recommendation: the platform's existing
   per-session staging dir (`<session dir>/.citadel-stage`, under
   `DefaultCobrowseSessionsBaseDir`). Every teardown path already removes it,
   including the orphan sweeps. Alternative: a new dir under `localStateDir`,
   which nothing sweeps after a crash.
3. **May the agent stage bytes it already has (a file it generated)?**
   Recommendation: not in v1. The agent tells the human which file to pick,
   and the human picks it in the viewer. Alternative, for later: a
   `local_cobrowse_stage_file` tool taking base64 bytes. It would stage
   nothing until the human accepts the file on a card showing name, size and
   sha256, and every attach of that file would still ask.
4. **Who attaches in v1.** Recommendation: the human, from the viewer's
   "page is asking for a file" card, whether or not they have taken control.
   The attach runs under the session write lock, and pressing Attach is the
   consent. Alternatives: require Take control first, as the node path (#1728)
   does; or also give the agent `set_file_input` in v1.
5. **Agent attach.** Recommendation: a later slice adds
   `local_cobrowse_set_file_input`, and every call asks, like send, pay and
   delete do today. It never rides a domain grant, and the approval is bound
   to the staged file's sha256, the page, the chooser and the frame URL.
   Alternative: never let the agent attach in local mode.
6. **A pending file chooser blocks agent writes.** Recommendation: yes, the
   same way a pending commit does. Reads still work, and the human can
   dismiss the request. Alternative: let the agent keep acting, and rely on
   the attach fences to refuse when the page moved.
7. **Forms that contain a file input.** Today the local probe marks any form
   with a file input as unverifiable. Every write inside such a form is then
   refused with "do it yourself", including typing into its text fields (see
   Findings). Recommendation: make that form state verifiable in v1. Each
   file entry binds by field name, file name, size, type and lastModified, so
   fills in the form ride the grant and its Submit is an ordinary
   always-ask commit. Alternative: leave such forms to the human, who takes
   control and completes them.
8. **Choosers inside a cross-site frame** (for example an identity
   verification widget embedded in a portal). Recommendation: refuse them in
   v1 with a clear reason ("this file field belongs to another site embedded
   in the page"). Cross-site frames run out of process, and the watcher would
   need auto-attach to see them. Alternative: support them with auto-attach
   and a card that names the frame's site, not the tab's.
9. **Edit and chord consent.** Recommendation: `type` with selector or clear,
   and the allowed chords, ride the existing per-domain write grant. These
   cases always ask:
   - Shift+Enter, which is treated as Enter;
   - Ctrl+Backspace, Ctrl+Delete and Shift with a printable key outside a
     text field, which are treated like the bare key;
   - any unresolved target.

   A selector target that is hidden is refused, and so is one that is a
   password or one-time-code field. Alternatives: make every `clear` ask; or
   allow password targets through selector, as plain `type` allows today.
10. **Is #1584 (CDP over a pipe) a prerequisite?** Recommendation: no. Upload
    does not create the loopback debug port exposure, but it raises its value,
    so #1584 should land before the quickstart advertises uploads.
    Alternative: block local upload on #1584.
11. **A local Files switch.** Recommendation: no separate setting. The human's
    pick in the viewer is the consent for each file. Alternative: an opt-out
    (`citadel local config` or an environment variable) for machines where
    uploads should be off.

## Findings from the code

These come from reading the current tree. The first slice that touches each
area confirms it with a test before changing it.

- **Local mode is excluded from the chooser machinery in three places.**
  `startChooserWatch` returns early for a tracked session.
  `HumanFileAttachSupported` requires `!trackTargets`. `HumanAttachStagedFile`
  refuses a tracked session. The watcher also dials `pickTargetContext` (the
  first page), while local mode follows popups and acts on the active page. So
  enabling it is new work, not a flag flip.
- **The local browser is headless or under Xvfb**, so a native file dialog
  never appears. Without interception, a page's chooser is cancelled silently.
- **Upload forms are refused today.** `FormSnapshotScript` sets `bad` when
  `new FormData(form)` holds a File entry. An empty file input contributes an
  empty File, so this happens whenever the form has any file input. `bad`
  leaves the target unresolved, `Classify` returns commit, and the pending
  commit is unverifiable. So in local mode the agent cannot type into, click
  or submit any part of a form that contains a file input. The same applies
  to a click on the file input itself when it sits in a form.
- **"upload" is a commit word** in `commitLabelWords`, so clicking a button
  labelled Upload already asks.
- **`localviewer` caps action bodies at 4096 bytes** (`handleAction`), so an
  upload needs its own route.
- **`localaudit.Record` has no file fields.** Name, size and sha256 need new
  fields.
- **`dispatchCommit` cannot replay `type`**, because typed text is never kept
  in an `actionRef`. A `type` that classifies as a commit is never
  approvable. That stays true, and it is the right answer.
- **#1584 is open.** The co-browse browser listens on a loopback CDP port. Any
  local OS user can connect to it and call `DOM.setFileInputFiles` with any
  path our user can read. That is pre-existing and independent of this design.

## Constraints

These were decided by Jason and are not reopened here.

- Local mode needs no account, sends no telemetry, makes no egress of its own,
  and needs no admin rights or Docker. `TestLocalMCPStdioMakesNoOutboundCalls`
  stays green.
- Grants and approvals come only from human-side viewer routes. No MCP tool
  can approve, stage on the human's behalf, or attach without a card.
- The agent never causes Citadel to read a path the agent supplied.
- File bytes and typed text are never logged. The audit records name, size and
  sha256 only.
- The limits match the node path: 25 MiB per file
  (`MaxCobrowseStagedFileBytes`) and 16 files per session
  (`MaxCobrowseStagedFiles`).
- Windows comes first (Chrome or Edge), then macOS and Linux.

## Q1. How the human supplies a file

**Recommendation: a picker and drop zone in the viewer page.**

The viewer page already runs in the human's default browser (Edge, Chrome,
Firefox, anything) and holds the viewer bearer. It gets one
`<input type=file>` and a drop zone over the staged-file list. The page reads
the chosen `File` with the standard File API and posts the bytes to a new
loopback route. The browser supplies only a base name (`File.name`), never a
path, and the OS dialog it shows is the human's own.

Why not a dialog launched by Citadel:

- On Windows it means a COM `IFileOpenDialog` from a console or stdio process.
  `citadel local mcp` is a child of the agent app, and may not own a window on
  the user's desktop.
- macOS needs `osascript`. Linux needs `zenity` or `kdialog`, which may be
  missing. Each one is another surface to secure, and none of them is
  available to a viewer on another browser.
- Citadel would be the process reading an arbitrary path, chosen in a dialog
  the page or the agent can time. The viewer route keeps the read on the
  human's side of the boundary.

**Where the bytes live.** They go in the platform's session staging dir
(`cobrowseSession.stagingDirPath`), not a new dir under `localStateDir`:

- `StageFile` creates the dir owner-only (0700, or the protected owner-only
  DACL inherited from the session base dir on Windows). It refuses a symlink,
  a junction or a group/other-accessible dir, writes create-only (`O_EXCL`),
  checks free space, and fails closed on the count.
- Every way a session ends removes it: Stop, a failed launch, the orphan
  sweep, an expired registry record, and `CleanOrphanedProfileSessions` for a
  crashed saved-profile session.
- `localStateDir` holds durable state (audit, owner lock, profiles), and
  nothing sweeps a session's leftovers from it after a crash.

For a saved-profile session, the staging dir lives inside the decrypted
working dir. Teardown already refuses to seal while removing staging fails.
An acceptance test must show that `.citadel-stage` is gone before `Persist`
seals the profile, and that the sealed blob never contains a staged file.

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
- The name goes through `platform.ValidateStagedName`. An invalid name is
  refused with its reason, and the card offers a rename field prefilled with
  the original name. A name already staged with the same sha256 is a no-op
  that returns the existing record. A name already staged with different
  content is refused ("already staged; remove it first").
- The service calls `StageFile`, which keeps the count, size and free-space
  checks exactly as on the node.
- The response is `{name, size, sha256}`. `DELETE
  /api/sessions/{id}/files/{name}` removes one staged file through a new
  platform `RemoveStagedFile` that uses the same validation as
  `resolveStagedFile`.

A page drop onto the screencast is not routed into the page in v1, and
`Input.dispatchDragEvent` with files is out of scope. The drop zone only
stages.

## Q2. Who may stage

**v1: only the human, through the viewer route.** Staging is local: nothing
reaches a site until an attach. So the act of choosing a file is consent to
stage it, not to send it.

**The agent may ask, but not supply.** v1 adds no request tool. The page's own
file chooser is the request: when the agent's click opens it, the viewer
raises the file card (Q5). The agent learns the state through
`local_cobrowse_status`, which shows the pending chooser and the staged files
as name and size (never sha256 or content), and waits with
`local_cobrowse_wait until=file`. If the agent needs a particular file, it
says so in its own chat with the user. Alternative: a
`local_cobrowse_request_file {session_id, reason}` tool that puts a
one-line, sanitized reason on the card even before a chooser exists. It adds
little in v1, since a request without a chooser has nothing to fill.

**Agent-provided bytes (decision 3).** Not in v1. Base64 bytes in a tool
argument never involve a path, so they do not break the path rule. But they
let an agent upload content the human never saw, which is the exfiltration
shape the always-ask attach exists to stop. If it is added later:

- the bytes are held in memory, not staged, until the human accepts them on a
  card showing name, size, sha256 and "provided by the agent";
- the staged record carries `origin=agent`, which every later card shows;
- the agent's call counts against the write budget, and the 25 MiB and 16-file
  limits apply as usual.

## Q3. Who may attach, and how it fits the commit model

**Attaching is the commit, not the later submit.** Once `DOM.setFileInputFiles`
lands, the page's `change` handler can read the file and `fetch` it to any
public origin. `EgressGuard` only stops non-public addresses, and a form
`action` says nothing about where script sends the file. So the consent
question is always "may this page have this file", and the card says so
plainly: "The page on <site> receives this file and can send it anywhere."

**Human attach (v1, decision 4).** New method:
`Service.HumanAttachFile(ctx, id, chooserID, stagedName)`.

- It is reached only from the viewer `attach-file` action, never from an MCP
  tool.
- It runs under the session's `wmu`. That serializes it against every agent
  write and against `ApproveCommit`, so it works whether the agent or the
  human is driving.
- The human's press on the card is in-the-moment consent, like
  `HumanNavigate`. No domain grant is needed, and the grant is not changed.
- It does not mark the session identity-bearing: attaching a file is not a
  sign-in. If the human attached while driving, the usual Hand back marks it
  as today.

On the platform it uses a new owner-attach entry that shares `fillFileInput`
with the two existing callers. Only the fences differ:

| Binding | Node agent (`SetFileInput`) | Node human (#1728) | Local owner attach (new) |
|---|---|---|---|
| Who may run it | agent, not while handed off | viewer with control | the local viewer route, under `wmu` |
| Page | pinned action page | first page | the chooser's target id (`ExpectTarget`), still active |
| Chooser | backend node id matches | same pointer, same node id | same chooser id (a generation counter), same node id |
| Frame | `expected_url` from the caller | URL captured at open | URL and loaderId captured at open |
| Driver | `driverFence` | `humanControlFence` | driver unchanged since the attach began |
| Requester | none | viewer ctx not done | HTTP request ctx not done |

**Agent attach (later slice, decision 5).** `local_cobrowse_set_file_input
{session_id, staged_name, selector?, goal}` targets the pending chooser by
default, or a `selector`.

- It is always a commit and never covered by a grant. `writeAction` probes the
  target and records a pending commit whose `actionRef` carries the staged
  name, the staged sha256, the chooser id or selector, the frame id, and the
  frame's browser-side URL (from `Page.getFrameTree`, so the page cannot
  influence it).
- The fingerprint covers all of these, plus the page URL. The card shows the
  file (name, size, first 12 hex of sha256), the frame's site and a
  screenshot.
- `ApproveCommit` re-resolves the staged file and refuses if the sha256
  differs. It then dispatches through `SetFileInput` with the captured frame
  URL as `expected_url`. The agent never supplies `expected_url` in local
  mode; the service does.
- An approval is good for that one attach only. A denial is sticky for that
  fingerprint, as today.

**Clicking the file input.** A click whose target is an `input[type=file]`, or
a `label` whose control is one, only opens a chooser, and the human must pick
the file anyway. So it is classified as fill (decision 7's slice). A button
named Upload stays a commit, because on many sites it sends files already
attached.

**Submitting a form with a file (decision 7).** Change `FormSnapshotScript` so
that a File entry no longer sets `bad`. It encodes as a tuple
`[name, "file", fileName, size, type, lastModified]`, read through
`File.prototype` getters captured before page script can matter (the same
`env` pattern the probe uses). Then:

- fills in that form are ordinary fills under the grant;
- the form's Submit is an ordinary commit whose content hash changes if the
  files change after approval;
- the commit card lists the attached file names and sizes, which the service
  matches to staged records.

The file content is not hashed in the page. The page already holds the bytes
and could send any of them, so hashing adds nothing at submit time. The trust
decision was the attach.

## Q4. Consent for type with selector or clear, and key chords

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

**Classification** (decision 9):

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

The file card appears when a chooser is pending on the active page. It raises
attention the way approval does, and the tab title shows "waiting for you".

```
+--------------------------------------------------------------+
| The page on example-portal.com is asking for a file          |
| Field: "Upload your ID"  (accepts one file)                  |
|                                                              |
|  Staged for this session                                     |
|   ( ) passport-scan.pdf      2.1 MB                [Remove]  |
|   (o) utility-bill.pdf       640 KB                [Remove]  |
|                                                              |
|  [ Choose a file from this computer ]   or drop it here      |
|                                                              |
|  The page receives the file you attach and can send it       |
|  anywhere. Attach only if you expected this request.         |
|                                                              |
|  [ Attach utility-bill.pdf ]   [ Not now ]                   |
+--------------------------------------------------------------+
```

- **The staged list** also shows under the session controls when no chooser
  is pending, with Remove buttons and a count ("2 of 16 files"). It shows name
  and size. sha256 is in a details row for anyone who wants it.
- **Attach** calls `attach-file`. On success the card closes and the status
  line says "Attached utility-bill.pdf to example-portal.com". A refusal shows
  the fixed reason (page changed, chooser closed, file removed) and keeps the
  card if the chooser is still pending.
- **Not now** calls `dismiss-file`. That clears the request, unblocks the
  agent, and tells it `declined` through `wait until=file`.
- **Hand back.** When the human has control the card works the same way. The
  hand-back flow is unchanged: Hand back to agent returns control, and the
  agent's `wait until=file` (or `until=resumed`) returns.
- **Agent attach (later slice)** uses the commit card: screenshot, the target
  highlighted, "Attach passport-scan.pdf (2.1 MB, sha256 3f9a1c...) to the
  file field on example-portal.com", then Approve or Deny.
- **Rendering.** File names come from the human's disk and may contain
  anything. They are rendered with `textContent` only, never markup, and
  truncated in the middle to 60 characters.

## Q6. Threats

**A hostile page.**

- *Fake choosers.* A page can draw its own "choose a file" UI inside the
  screencast. The real card lives outside the picture, in the viewer's own
  chrome, and is raised only by the browser's `Page.fileChooserOpened` event.
  The human only ever picks files in the viewer's picker. A page can also
  call `input.click()` from any handler the agent's click triggers. The card
  then appears unasked, which is why it names the site, the field label and
  the warning line, and why the agent is blocked while it is up.
- *Prototype spoofing.* Resolution and `checkFileInput` run in an isolated
  world (`Page.createIsolatedWorld`). The frame URL comes from
  `Page.getFrameTree`, never from page script. File entries in the form
  digest are read through prototype getters captured by the probe.
- *Navigating mid-attach.* The #1728/#1731 lesson applies: every awaited CDP
  read is followed by a re-check of every binding, with nothing awaited
  between the last check and `DOM.setFileInputFiles`. The bindings are:
  - the session is alive, on the same port and browser id;
  - the chooser id is the same;
  - the target is still active;
  - the frame URL and loaderId are unchanged;
  - the driver is unchanged;
  - the request context is not done;
  - `wmu` is held.

  A main-frame navigation, a target switch or close, or a new chooser clears
  the pending one. A same-URL reload changes the loaderId, which the URL check
  alone would miss.
- *Swapping the input.* A replaced element is a different backend node and
  fails the chooser match. After an attach, a page can swap `input.files`
  with its own File. It only fools itself: it already had the bytes.
- *Cross-site frames.* Refused in v1 (decision 8). The card would otherwise
  name the tab's site while the file goes to the frame's.

**A hostile or confused agent.**

- *Exfiltration by upload.* The agent cannot stage bytes (v1), and it cannot
  attach without the human's press (v1) or a per-upload approval (later). The
  card names the receiving site. It cannot stop a human who is socially
  engineered into attaching a sensitive file, and the warning line is the
  mitigation. After an attach the agent can read whatever the site renders of
  the file. That is ordinary page reading of a site the human chose to give
  the file to.
- *A path from the agent.* No tool takes a path. Staged names pass
  `ValidateStagedName` and resolve only inside the session's staging dir
  (`resolveStagedFile`: regular file, no symlink, the real dir equals this
  session's).
- *Huge or many files.* There are 25 MiB and 16-file limits, checked server
  side, and the size is refused before the body is read. Uploads are
  serialized, free space is checked, and the count fails closed. Only the
  human uploads.
- *Prompt spam.* Only one chooser is pending at a time, and its card expires
  after `PendingCommitTTL` (10 minutes). Writes are blocked while it is up,
  so an agent cannot stack requests.
- *Chord abuse.* `CheckKeyChord` refuses clipboard chords, so the agent cannot
  paste or copy through the browser. Shift+Enter and editing chords outside a
  field ask (Q4).
- *Writing into fields the human cannot see.* Selector targets must be visible
  (Q4).

**Other local users on the same machine.**

- *The loopback viewer port.* Every OS user can reach 127.0.0.1. The cookie
  alone unlocks nothing, because browsers send it to every 127.0.0.1 port.
  The bearer exists only in the `/open` page's sessionStorage, minted from a
  single-use code in an owner-only launch file. The upload and attach routes
  check both before reading a body. Without the bearer, another user gets a
  403 and nothing is buffered beyond the request headers.
- *File permissions.* Unix: the session base dir must be owned by our uid and
  not group/other-writable. The staging dir is 0700, files are 0600, and
  `existingStagingDir` refuses anything group/other-accessible. Windows: the
  base dir requires a protected owner-only DACL
  (`validateCobrowseBaseDirPerms`), and the staging dir and files inherit it.
  A windows-latest test reads the DACL of a staged file and requires that only
  the owner (plus SYSTEM and Administrators if inherited by policy) has
  access.
- *The debug port (#1584).* Another user can drive the browser over its
  loopback CDP port. That includes `DOM.setFileInputFiles` with any file our
  user can read. This exists today without upload. #1584 closes it, and
  decision 10 covers the ordering.

**Audit.** `localaudit.Record` gains `file_name`, `file_size` and
`file_sha256`.

- Events `stage_file`, `remove_file`, `file_attach` and `file_dismiss` are
  Class H, actor user. Agent attach requests and approvals reuse the commit
  events.
- URLs go through the existing origin-plus-path-hash reduction.
- Error text is a fixed reason, never a path.
- `TestLocalAuditRecordsEveryCallWithoutSecrets` extends with a sentinel in
  the file bytes and in the session dir path, and neither may appear anywhere
  in the log.

## Q7. Slices

In dependency order. Each one is sized for one agent. "Opus review" marks
browser-control or security slices. Real-browser tests run on Linux and on
windows-latest with both Chrome and Edge.

**S1. Chooser watch for tracked (local) sessions.**
- Area: `internal/platform`. Opus review (browser-control).
- Scope:
  - lift the `s.tracking` early return;
  - attach the watcher to the active target, and redial on a target switch
    or close;
  - key the pending chooser by target id, with a generation counter as its
    chooser id;
  - record the frame URL and loaderId;
  - clear it on a main-frame navigation, a target change or session stop;
  - record a subframe chooser whose frame site differs from the top page's
    with `cross_site: true`, so S2 can refuse it.
- Acceptance:
  - fake-CDP unit tests for each transition;
  - real browser: a chooser opened on the active page is recorded, a tab
    switch clears it, a chooser in a popup after the switch is recorded, and
    a reload clears it.

**S2. Owner attach on the platform.**
- Area: `internal/platform`. Opus review (security).
- Scope:
  - an owner-attach entry that shares `fillFileInput` and uses the local
    fence set (Q3 table);
  - `RemoveStagedFile`;
  - the local runtime wires `SetFilesAllowed` to always allow (decision 11),
    but local mode attaches only through the viewer HTTP action. The stream's
    `file_attach` input stays off for a tracked manager, and the existing
    fail-closed test is replaced by one that pins exactly that.
- Acceptance:
  - fake-CDP tests in which each fence fails in turn after each awaited
    read, and nothing is dispatched;
  - a cross-site chooser is refused;
  - a wrong chooser id is refused;
  - real browser: an attach after a same-URL reload is refused, and an
    attach to a live chooser delivers the bytes and the page sees the staged
    name.

**S3. Service: staging, attach, dismiss, blocking, status, audit.**
- Area: `internal/localcobrowse`, `internal/localaudit`, `cmd/mcp_local_cobrowse.go`.
  Opus review (security).
- Scope:
  - `HumanStageFile`, `HumanRemoveFile`, `HumanAttachFile` and
    `HumanDismissFile`, all under `wmu`;
  - `ErrFileRequested` for agent writes while a chooser is pending;
  - `wait until=file` (attached, declined, changed, expired);
  - agent status fields: `file_chooser` and `staged_files` (name and size);
  - the new audit fields;
  - the `Browser` interface and `fakebrowser` extended.
- Acceptance:
  - unit tests on the fake browser;
  - `TestNoLocalMCPToolCanApprove` extended so that no tool reaches stage,
    remove, attach or dismiss;
  - the audit sentinel test (Q6);
  - `TestLocalMCPStdioMakesNoOutboundCalls` stays green.

**S4. Viewer routes and UI, plus end-to-end acceptance.**
- Area: `internal/localviewer`, `cmd`. Opus review (security).
- Scope:
  - the upload and delete routes (Q1);
  - the `attach-file` and `dismiss-file` actions;
  - the `app.js` card, staged list and drop zone;
  - local runtime wiring.
- Acceptance:
  - guard tests: no cookie, no bearer, a foreign Origin or a missing Origin
    each give 403 with the body unread; over-limit Content-Length gives 413;
    a 17th file is refused; a bad name is refused; a duplicate is handled;
    a second concurrent upload gets 409;
  - real browser end to end: the viewer uploads, the page opens a chooser,
    the card appears, Attach runs, and the test page receives bytes whose
    sha256 matches;
  - staging is gone after `local_cobrowse_stop` and after `HumanStop`;
  - a saved-profile session's sealed blob never contains a staged file;
  - windows-latest DACL check on a staged file;
  - a non-ASCII file name round-trips on Windows.

**S5. File-aware form digest and file-input clicks.**
- Area: `internal/platform/cobrowse_target.go`, `internal/localcobrowse/commit.go`.
  Opus review (security).
- Scope:
  - file entries encoded in the digest (Q3);
  - a click on a file input or its label classified as fill;
  - the commit card lists the attached files;
  - `RuleVersion` bumped.
- Acceptance:
  - first, a real-browser test that pins today's refusal of a text fill
    inside a form with a file input, then flips;
  - `formsnapshot_test` cases: an empty file input, an attached file, files
    swapped after approval (the fingerprint changes), and spoofed `File`
    prototype getters;
  - commit browser test: fill and submit an upload form after a human
    attach, with the card listing the file.
- Order: independent of S1 to S4, but needed before an upload form is usable.

**S6. Type with selector and clear, and key modifiers, in local mode.**
- Area: `internal/localcobrowse`, `cmd/mcp_local_cobrowse.go`. Opus review
  (browser-control). Starts after PR #1731 merges.
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
- Order: independent of S1 to S5.

**S7. Agent `local_cobrowse_set_file_input` (decision 5).**
- Area: `internal/localcobrowse`, `cmd`. Opus review (security). After S3
  and S5.
- Scope:
  - always-ask commit, with an `actionRef` and fingerprint over the staged
    sha256, the target, the chooser or selector, and the frame URL;
  - `ApproveCommit` dispatch through `SetFileInput`, with the
    service-supplied `expected_url`.
- Acceptance:
  - the grant never covers it;
  - a re-staged file with a different sha256 makes the approval "changed";
  - a navigation between approval and dispatch makes it "changed";
  - a denial is sticky;
  - real browser approve-and-deliver.

**S8. Docs.**
- Area: `docs/local-browse-quickstart.md`, `local doctor` text. Sonnet.
- Scope: the upload flow for Windows users, the warning line, the limits,
  and the #1584 note if it has not landed.

## Not covered

- Uploads through drag-and-drop onto the page (`Input.dispatchDragEvent`).
- More than one file per attach for a `multiple` input. v1 attaches one file
  per press, as the node path does. Multi-select of staged files is a small
  follow-up.
- Choosers on background pages. Only the active page is watched, which
  matches what the human sees and where actions go.
- A cloud-viewer equivalent. The node and cloud path is #1723 and
  aceteam#13831.
