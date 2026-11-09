# Release notes draft: Citadel local mode (local browse)

Paste into the GitHub release body for the first release that includes the
aceteam#13503 local-mode stack (citadel-cli #1569, #1570, #1572, #1573 and this
slice).

## New: Citadel local mode

An AI agent app on your computer can now drive a browser on the same computer
through Citadel, with no AceTeam account, no Docker and no admin rights.
Windows comes first. See the
[local browse quickstart](https://github.com/aceteam-ai/citadel-cli/blob/main/docs/local-browse-quickstart.md).

- **`citadel local mcp`.** A stdio MCP server that serves only local tools,
  including the new `local_cobrowse_*` tools: start, status, navigate, click,
  type, select, key, scroll, extract, screenshot, handoff, resume, stop and
  wait. It never reads an API key or device token, and it makes no network
  calls of its own (no update check, no telemetry).
- **Windows browser runner.** Uses the installed Microsoft Edge or Google
  Chrome in a hidden (headless) window, so it never takes over your screen. A
  Windows Job Object ties the browser to the Citadel process, so no browser
  outlives it.
- **Local viewer.** A page on `127.0.0.1` where you watch the session, approve
  or deny the agent's actions, take control to log in, and hand back.
  `citadel local viewer` reopens it.
- **Approvals.** The agent's clicks, typing and navigation need your approval
  in the viewer, for 24 hours at most and per session. Every agent browser
  action is refused while you have control.
- **Audit log.** Every action, refusal and approval is recorded in a
  hash-chained log on this computer. Typed text is never recorded. Use
  `citadel local audit` to read it and `citadel local audit --verify` to check
  it.
- **Limits.** At most 2 sessions at once, 120 actions a minute and 600 page
  loads an hour per session, and an 8-hour idle lifetime. Local and private
  network addresses are refused.
- **`citadel local doctor` and `citadel local config <copilot|claude-desktop|codex>`.**
  First-run checks, and the exact MCP entry for your agent app, written for
  you with `--write`.

Plain `citadel mcp` also serves the `local_cobrowse_*` tools for the machine it
runs on.

**Known limits in this release:**

- Releases are not code-signed yet. On first run, Windows SmartScreen asks you
  to confirm (More info, then Run anyway).
- Third-party sign-in popups and extra tabs are not shown in the viewer.
- Logins are not remembered between sessions yet. Each session uses a
  throwaway profile.
- macOS and Linux local mode are not supported yet. On Linux, co-browse
  still needs Xvfb.

Citadel is source-available under the Elastic License 2.0, and free to use and
self-host.
