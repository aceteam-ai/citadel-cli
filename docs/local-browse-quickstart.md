# Citadel local browse: quickstart

Citadel local mode lets an AI agent app on your computer drive a web browser on
the same computer, with you in charge:

- **You hand off and take back.** When the agent reaches a login or payment
  page, it hands the browser to you. You type into the page yourself in the
  Citadel viewer, then press **Hand back to agent**. The agent never sees what
  you type.
- **Approvals and an audit trail.** The agent can look at pages without
  asking, but it needs your approval in the viewer, for each website, before it
  clicks, types or navigates there. Sending, posting, submitting a form, paying
  or deleting always asks you again, every time. Every action is recorded on this computer, and
  `citadel.exe local audit` shows it.
- **It stays on your computer.** There is no AceTeam account. Docker and admin
  rights are not needed. Citadel makes no calls of its own: no update check
  and no telemetry. (The setup page can optionally connect this PC to AceTeam;
  it does so only when you click that button.) The co-browse browser's connections go through a guard
  inside Citadel that only connects to public internet addresses, and the
  browser's own background updates and syncing are turned off.

Citadel is source-available under the Elastic License 2.0, and free to use and
self-host. This page covers Windows 10 and 11 first. macOS and Linux are coming.

## Let your agent set it up

If you use an agent that can run commands on your computer (Codex or Claude
Code), paste the text below into it. It installs Citadel, checks it, connects
itself, and tells you when to restart the app. You still do two things: click
through the Windows SmartScreen prompt, and restart the app at the end.

Using Codex? Before you paste, type `/permissions` and choose "Ask for
approval" (on first run, a standard non-admin user picks "non-admin sandbox").
Codex will then ask you before each step that needs the internet or files
outside its folder; approving those is expected.

```text
Set up Citadel local browse on this Windows computer, so you can drive a web
browser with me. Use PowerShell. You do not need admin rights; do not use them.

1. Install Citadel for the current user:
   iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex
   The program is installed at "$env:LOCALAPPDATA\Citadel\citadel.exe". Use that full path
   below, because this window may not see the updated PATH yet. Ignore the
   installer's closing "next steps" text.
2. Windows may show a "Windows protected your PC" (SmartScreen) prompt the first
   time Citadel runs, even during step 1. You cannot see the dialog. If a command
   hangs with no output, or fails as blocked, STOP and ask me to click "More
   info", then "Run anyway". Do not try to get around it. Continue after I say
   it is done.
3. Run: & "$env:LOCALAPPDATA\Citadel\citadel.exe" local doctor
   Each line is ok, note or FAIL. Fix any FAIL whose fix is a command you can run
   without admin rights. If a FAIL needs something else (a missing browser,
   admin rights, a blocked file), STOP, tell me exactly what it said, and let me
   handle it.
4. Connect yourself. Use "codex" if you are Codex, or "claude-code" if you are
   Claude Code:
   & "$env:LOCALAPPDATA\Citadel\citadel.exe" local config <codex or claude-code> --write --yes
   It keeps a backup of the file it changes. If it refuses (for example, a
   different "citadel" entry is already there), STOP and show me the message.
   Never add --replace yourself.
5. Tell me to fully quit and reopen this app, then ask you to browse a site, for
   example: "Using citadel, open wikipedia.org and tell me the page title." I
   will approve the website in the Citadel viewer that opens. Do not ask me for
   passwords: when a page needs a login, hand the browser to me and wait.
6. For the browsing steps, use only Citadel's tools. If Citadel reports an
   error, show me the error. Do not switch to another tool or work around it.
   If Codex asks to approve a Citadel tool, that is expected: it asks once per
   tool, and "Allow for this session" is fine.
```

Claude Desktop chat cannot run commands, so it does not use this block (Desktop's
Claude Code mode can: have it use `claude-code`). For the chat: double-click
`citadel.exe` and press **Connect** beside Claude Desktop (see
[Fastest path](#fastest-path-double-click)).

## What you need

- Windows 10 or 11 (x64).
- Microsoft Edge, which comes with Windows, or Google Chrome, version 109 or
  newer. `citadel.exe local doctor` checks this.
- One MCP-capable agent app. These are covered below:
  - the GitHub Copilot CLI (VS Code keeps its MCP servers in its own
    `mcp.json` file, which this guide does not cover);
  - Claude Code;
  - Claude Desktop;
  - Codex;
  - opencode.

## 1. Install

Download `citadel_<version>_windows_amd64.zip` from the
[GitHub Releases page](https://github.com/aceteam-ai/citadel-cli/releases).
Unzip `citadel.exe` into `%LOCALAPPDATA%\Citadel`.

Or run the per-user installer in PowerShell. It needs no admin rights:

```powershell
iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex
```

**SmartScreen.** Releases are not code-signed yet. The first time you run
`citadel.exe`, Windows may show "Windows protected your PC". Click **More
info**, then **Run anyway**. If your antivirus quarantines the file, restore it
and allow it; signed releases are planned.

## Fastest path: double-click

Double-click `citadel.exe` (or run `citadel.exe local setup` in any terminal, on
any OS). Your default browser opens a setup page served from your own
computer, behind a one-time link:

1. It shows the same checks as `local doctor`, in plain language. **Run the
   full check** also starts a hidden test browser.
2. It lists the agent apps it finds. Press **Connect** beside one. That click is
   your permission to add Citadel to the settings file shown next to the
   button; your other settings are kept and a backup is saved beside the file.
   The page then tells you what to do next, for example "restart Claude Desktop,
   then ask it to browse a site". An app that is not installed says "not
   found". For any other app, including Muse, copy the generic MCP entry at the
   bottom of the page into that app's MCP settings.
3. It lists your live browser sessions and the session limit (read-only), and
   **Open the viewer** opens the hand-off viewer.
4. Optional: **Connect this PC to AceTeam**. You do not need it for local
   mode. Press **Connect**, and an AceTeam page opens in your browser (or scan
   the QR code with your phone). Enter the code the setup page shows, then
   approve. The PC joins the account and organization you approve it in, and
   the setup page then shows the PC's name and that organization. **Cancel**
   stops it, unless the last step has already started. Connecting saves a
   device key only inside your own user folder; if Citadel on this PC was set
   up for all users, the page refuses and you use `citadel enroll` instead.
   The PC stays registered after you close the window, but AceTeam agents can
   use it only while Citadel's worker runs. Your cookies and saved logins stay
   on the PC. If the page says the PC already has a registration (for example
   after a try that did not finish), run `citadel logout` in a terminal and
   press **Connect** again.

The page also opens for any launch that gives `citadel.exe` a console of its
own, such as `start citadel` or `Start-Process citadel`, not only a double-click.

Keep the console window open while you use the page; closing it stops the page.
The steps below do the same things from a terminal.

## Link a folder on Windows

This is the account-connected file path, not local-only browsing. It requires
an AceTeam account, a connected node and a running worker. The default folder
is `%USERPROFILE%\citadel-node\workspace`; this is not a folder-sync service
or a picker that grants access to every folder on the PC.

1. Install the current Windows x64 release using [Install](#1-install).
   Releases are unsigned: verify the download came from the linked release
   page before handling a SmartScreen warning yourself. If Windows or an
   organizational policy blocks it, stop and ask your administrator; do not
   disable protection or ask an agent to bypass it. Administrator rights are
   not required for the per-user installer.
2. Open `citadel.exe local setup` and choose **Connect this PC to AceTeam**.
   Approve the displayed code in the intended account and organization.
   Connecting through this page initially leaves Files off.
3. On the setup page, enable **Allow agents to read and upload files in**.
   The path beside that checkbox is the workspace to use. Leave commands and
   other permissions off unless you separately need them. Put a disposable
   test file in that folder using File Explorer (create the folder if needed);
   do not use sensitive files
   for the first check. Copying a file here creates a separate copy, not a
   live link to its original location. A custom worker `--workspace` or
   `CITADEL_WORKSPACE` setting can change the root, so do not assume the
   default if the worker was configured separately.
4. Choose **Keep this PC available to AceTeam**. Check that both the sign-in
   entry and background-worker steps succeed. Registration alone does not
   keep the node online; the worker must be running. If a step fails, keep
   its error message and stop rather than treating the node as ready.
5. Select this node in your flow and try `NodeFileList`, then `NodeFileWrite`
   with a new test filename. Through the account-connected MCP, try
   `code_list`, `code_read`, `code_write` and `code_search` against the same
   node and workspace. Read back the written contents before using real
   files. Writes can overwrite an existing file; use backups. Files outside
   the workspace are not granted by this checkbox; do not widen access to
   work around a refused path.
6. Sign out and back in, or reboot and sign in again. Confirm the node returns
   online and repeat the test. **Start at sign-in** does not mean it runs
   before anyone signs in. Turn Files off on the setup page when no longer
   needed. To disconnect, choose **Disconnect this PC** and check that every
   cleanup step succeeds; a partial result needs attention before retrying.

These are setup and verification steps, not a claim of completed Windows
end-to-end acceptance. A successful install or local browser check alone does
not verify a flow/MCP round trip or persistence after sign-in.

## 2. Check

```powershell
& "$env:LOCALAPPDATA\Citadel\citadel.exe" local doctor
```

To save typing in the steps below, keep the path in a variable for this
PowerShell window:

```powershell
$citadel = "$env:LOCALAPPDATA\Citadel\citadel.exe"
```

Each line is `ok`, `note` or `FAIL`, and every `FAIL` names a fix. The checks
are:

- the browser Citadel will use;
- a hidden test launch;
- the local viewer port;
- the saved-logins passphrase, which is only needed to remember logins;
- other agent apps using co-browse;
- the audit log;
- which agent app configs already point at Citadel.

## 3. Connect your agent app

Print the entry for your app. It contains the full path to `citadel.exe`,
because apps started from the Start menu may not see an updated `PATH`.

```powershell
& $citadel local config copilot          # Copilot CLI: ~\.copilot\mcp-config.json
& $citadel local config claude-code      # ~\.claude.json (user scope)
& $citadel local config claude-desktop   # %APPDATA%\Claude\claude_desktop_config.json
& $citadel local config codex            # ~\.codex\config.toml (or $env:CODEX_HOME)
& $citadel local config opencode         # ~\.config\opencode\opencode.json
```

Add `--write` to have Citadel merge the entry into that file for you. It asks
first, keeps your other settings, and saves a backup next to the file.

For GitHub Copilot and Claude Desktop, the entry is JSON like this:

```json
{
  "mcpServers": {
    "citadel": {
      "command": "C:\\Users\\YOU\\AppData\\Local\\Citadel\\citadel.exe",
      "args": ["local", "mcp"]
    }
  }
}
```

Copilot's entry also carries `"type": "local"` and `"tools": ["*"]`.

Claude Code's entry goes in the same place `claude mcp add --scope user citadel
-- <path to citadel.exe> local mcp` writes it: the top-level `mcpServers` of
`~\.claude.json`. It also carries `"type": "stdio"` and `"env": {}`. That file
holds all of Claude Code's other settings, which `--write` leaves untouched. If
an entry named `citadel` already differs, `--write --yes` refuses; add
`--replace` to overwrite it. If `CLAUDE_CONFIG_DIR` is set only in Claude Code's
own settings (`env`), the double-click setup page does not see it.

For Codex, the entry is TOML:

```toml
[mcp_servers.citadel]
command = 'C:\Users\YOU\AppData\Local\Citadel\citadel.exe'
args = ['local', 'mcp']
```

For opencode, the entry goes under `mcp.servers` and the command is one list
(the executable, then its arguments):

```json
{
  "mcp": {
    "servers": {
      "citadel": {
        "type": "local",
        "command": ["C:\\Users\\YOU\\AppData\\Local\\Citadel\\citadel.exe", "local", "mcp"]
      }
    }
  }
}
```

This needs opencode V2. The file is `opencode.jsonc` if it exists, otherwise
`opencode.json`, in `$OPENCODE_CONFIG_DIR`, else `$XDG_CONFIG_HOME/opencode`,
else `~/.config/opencode`. `--write` only edits plain JSON: a
file with comments or trailing commas, or one using opencode's older flat `mcp`
layout, is left alone and you add the entry by hand. opencode can use any
OpenAI-compatible endpoint as its model provider, including one you run
yourself.

Restart the agent app after changing its config.

## 4. Use it

Ask your agent something like: "Using citadel, open the site where I keep my
orders, let me log in, then list my last three orders."

1. **Start.** The agent calls `local_cobrowse_start`. The Citadel viewer
   opens in your default browser at `http://127.0.0.1:<port>/` and shows
   "Agent driving". The co-browse browser itself runs hidden. It is separate
   from your own browser and never uses your normal browser profile.
2. **Approve.** On the agent's first click or navigation, the viewer asks
   whether to allow this agent to click, type and navigate on that website
   (and its subdomains). You can allow it for 8 hours (the default), 24 hours or
   1 hour, or deny it; the approval ends when the session ends. Once you have
   taken control of a session, for example to log in, approvals in it last at
   most 1 hour and end after 15 minutes without activity. Each Send, Post,
   Submit, Pay or Delete asks you separately, with a picture of the button, and
   approving does that one action right away (if the page is still as shown), and your answer covers it only. You can revoke approvals at any
   time.
3. **Log in yourself.** At the login page the agent hands off. The viewer
   shows "Waiting for you: log in, then hand back". Press **Take control**,
   click into the page and type your password. Then press **Hand back to
   agent**. While you drive, the agent cannot see the screen or act.
4. **Watch the record.** The Activity panel lists every action. Your typed
   text never appears there, only how many characters the agent typed.
5. **Stop.** When the agent calls `local_cobrowse_stop`, or you press **End
   session**, the browser's cookies and logins for that session are deleted.

### Remember logins on this computer

By default every session starts logged out and forgets everything when it
stops. To keep a login, ask the agent to use a saved profile, for example
"use the citadel profile named default". The session then starts **locked**:

1. The viewer asks for your Citadel passphrase. The first time, it asks you to
   choose one. Use four or more random words. A short PIN is refused, because
   it would not protect your logins if someone copied this computer's disk.
2. The agent never sees the passphrase and is not told it; it only learns that
   the session went from locked to running. The passphrase is not recorded.
3. While the session runs, the profile is decrypted into a private folder for
   your user. When the session stops, it is encrypted again and the plain
   copy is deleted.
4. If you would rather not unlock, press **Continue without saved logins**.
   The session then starts logged out and nothing is saved. Citadel never
   does this on its own.

Logins are saved when the session stops normally: the agent stops it, you
press **End session**, or the agent app closes Citadel. If the agent app is
killed outright, logins from that session are not saved; the copy saved last
time is kept.

If you forget the passphrase, the saved logins cannot be recovered by anyone,
including AceTeam. `& $citadel local passphrase forget` deletes them so you
can start again. It also removes a decrypted copy left behind by a session
that was cut off (for example when the agent app was killed). If such a copy
is still in use or cannot be removed, it deletes nothing and names the folder. Other commands:

```powershell
& $citadel local passphrase status         # set or not, saved profiles, lockout
& $citadel local passphrase change         # new passphrase, logins kept
& $citadel local passphrase reset-lockout  # after too many wrong tries
```

This passphrase is only for saved logins. It does not give access to this
computer, and it is separate from an AceTeam node's master PIN.

If you close the viewer tab, reopen it (in a PowerShell window where you set
`$citadel` as above) with:

```powershell
& $citadel local viewer
```

The command opens a fresh one-time link.

## Limits

- At most 2 sessions run at once.
- In one session, the agent gets at most 120 actions a minute and 600 page
  loads an hour.
- A session ends when it is stopped, when the agent app closes its Citadel
  server, or after 8 hours without activity.
- The agent cannot open addresses on your computer or private network, and
  only `http`/`https` pages are allowed.
- There is no batch or URL-list tool, and none for proxies or for solving
  CAPTCHAs.
- On macOS the browser runs hidden with `--use-mock-keychain`, so it never asks
  for your login Keychain. While a session runs, the decrypted working copy of
  a saved-login profile uses Chromium's mock key (the same posture as the Linux
  basic password store). The sealed profile at rest is unchanged: it is still
  encrypted with your passphrase.

## When something goes wrong

| What you see | What to do |
|---|---|
| "no supported browser found" | Install Microsoft Edge or Google Chrome. The message lists every place Citadel looked. |
| "organization policy blocks automation" | A work or school computer can forbid browser automation. Use a personal computer or a per-user Chrome install. |
| "exited during headless launch" | Run `& $citadel local doctor`. If it repeats, restart the computer. |
| The viewer says "No agent connected" | The agent app has not started Citadel. Check its config with `& $citadel local doctor`, then restart the app. |
| "another Citadel process already owns co-browse" | Another agent app is using co-browse. Close it, or use that one. |
| `FAIL Self-update`: the binary is owned by Administrators | The installer ran from an elevated PowerShell and left `citadel.exe` owned by Administrators, so `citadel update install` is refused. From an elevated PowerShell run `icacls "$env:LOCALAPPDATA\Citadel\citadel.exe" /setowner "$env:USERDOMAIN\$env:USERNAME"` (use the account that runs Citadel), or re-run the installer. |
| `approval_denied` / `approval_expired` / `approval_revoked` | The agent was told your answer. Approve again in the viewer if you want it to continue. |
| `commit_approval_required` / `commit_denied` | A send, post, submit, payment or delete needs your answer each time. The viewer shows the button; the agent is told if you decline. |
| "navigation refused" | The agent tried a local, private or non-web address. This is deliberate. |
| `rate_limited` | The agent is acting too fast. It should slow down. |
| The viewer link "expired or was already used" | Run `& $citadel local viewer`. |
| `locked` | The session uses saved logins. Unlock it in the viewer, or press **Continue without saved logins**. |
| "Too many wrong passphrases" | Wait for the time shown, or run `& $citadel local passphrase reset-lockout`. |
| A sign-in popup or a second tab does not show | Popups and extra tabs are not shown in the viewer yet. Use a site's email-and-password login. |

## What it protects, and what it does not

Local mode protects against:

- the agent making mistakes;
- web pages trying to steer the agent (prompt injection), including toward
  the viewer;
- web pages in your own browser trying to reach Citadel's local ports;
- other user accounts on the same computer;
- someone who copies this computer's disk, for saved logins only, because
  they are encrypted with your passphrase.

It does not protect against a hostile program running as **your** user
account, including an agent app with shell access that decides to misbehave.
Such a program can already read your files. This is the same boundary as any
local agent tool.

`& $citadel local audit --verify` checks that the audit log was not edited. That
detects tampering. It does not prove anything against a program running as
your account.

Local mode keeps its state (the co-browse lock, the audit log, the viewer's
link file, and the encrypted saved logins with their passphrase vault) in `%LOCALAPPDATA%\Citadel\local` (`~/.citadel-cli/local` on
macOS and Linux), separately for each user, even on a computer that also runs
an AceTeam node. Audit records written by v2.212.0 and v2.213.0 stay in the
older location and `local audit` no longer shows them.

## Later: connect to AceTeam

Local mode does not need AceTeam. If you later connect this computer to
AceTeam (with the setup page's Connect button, or `citadel init`), cloud agents still cannot reach the sessions your
local agent app starts. Your local approvals apply only to local agents, never
to cloud agents.
