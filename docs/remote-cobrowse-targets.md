# Pages in remote co-browse sessions

New `COBROWSE_SESSION` sessions on the node-wide manager track up to eight page
targets, using the same opener stack as local sessions. A popup opened by the
active page becomes active. When it closes, the manager returns to its opener.
The viewer and scripted actions share this page set.

## Compatibility and capability discovery

`COBROWSE_SESSION` with `action: capabilities` returns `features: ["tabs"]` when
the manager supports the arguments below. A tracked session's status also
includes this feature. A legacy session can exist on a capable manager, so
callers must inspect the individual session's `features` before using tab IDs.

Each registry record persists its session's tracking mode. Records written before
this change omit the bit and retain single-page behavior after adoption. Enabling
tracking on a manager does not retrofit sessions it already owns. Stop and start
the session to opt an old session into the new behavior. Isolated managers remain
opt-in through `EnableTargetTracking`.

## Job inputs and results

`target_id` has two meanings:

| Action | Meaning |
| --- | --- |
| `navigate`, `click`, `type`, `select`, `key`, `scroll`, `handoff`, `resume` | Assert the currently active page. A stale assertion returns `ErrTargetChanged`; it never selects another page. |
| `screenshot`, `extract` | Read that approved page without selecting it. Omit the field to read the active page. |

The handler serializes dispatched page actions per session and restores the prior
assertion on success or failure. A viewer can still switch pages while an action
prepares; a final pin check refuses input when the active page has changed. Reads
check both before and after CDP, discarding a result if its page disappears or is
refused in the meantime. Existing human handoff continues to refuse agent reads
and writes. Passing a tab argument to a legacy session returns
`ErrTargetsDisabled` instead of silently ignoring it.

Start, individual status, list entries and every successful session action include:

- `active_target`: the active page ID, or an empty string when none is usable.
- `targets`: page descriptors with `id`, `title`, `url`, `active`, `refused` and
 `opened_by`.
- `open_targets`: the length of `targets`.
- `features`: `["tabs"]`.

Screenshot and extract also identify the page read as `target_id`. A background
screenshot's `url` belongs to that page; `active_target` still names the active
page. Stop returns an empty page set after teardown. Reset, capability discovery,
and ended-session tombstones have no live page to describe. Errors retain the
existing job error channel. Legacy session results retain their single-page shape.

`opened_by` is display provenance, never authority. The first observed page is
`start`; later browser-created pages are `page`. `RecordTargetOpenedBy` allows a
future page-creation caller to mark an existing approved page `agent` or `user`.
This slice adds no agent/user tab-creation action. Provenance entries are bounded
by the live page set and removed when their page closes. Recovery reconstructs
page provenance from discovery; it does not persist opener history.

## Viewer and navigation policy

The node-wide stream adapter offers `TargetSession`. The handler checks the
addressed session's tracking mode before sending `targets` frames or honoring
`target_switch` / `target_close`. Those messages keep their existing `target`
field and explicit human-control requirement. Legacy sessions continue through
the single-page stream path.

The production remote policy shares the existing public-address URL checker with
local mode: http, https and about:blank only, no embedded credentials, and no
loopback, private, link-local, mesh, metadata or other reserved addresses, including
numeric aliases and mapped addresses. Initial URLs are checked before launching
a browser. Explicit agent navigation is checked before `Page.navigate`. Discovery
closes refused pages, quarantines a refused last page, and refuses input and reads
there; navigating away to an approved address remains possible.

Discovery is an after-observation control, not a request interceptor. This change
does not claim to prevent an initial popup request, redirect, subresource request
or DNS rebinding from reaching the network. Before-load enforcement is the
separate #1668 investigation. Existing egress controls remain necessary.

## Verification

All local checks use fake launcher/CDP endpoints, isolated HOME and XDG directories,
and blocked external networking. They exercise the actual job handler and mounted
viewer handler. No native browser, node service, restart or deployment is required.
The `TestTargetSetRemoteDispatch*` tests use the existing Windows-portable
`TestTargetSet` CI selection; the stream adapter tests keep the existing
`TestNodeWideManagerSession` selection. Linux CI runs the full unit suite.

Real browser acceptance remains operator-owned: a popup posts a value to its opener
and closes, the viewer and next action return to the opener, background reads do
not change selection, and a flood stays bounded. Source tests simulate the target
transitions and CDP results; they do not substitute for that native acceptance.
