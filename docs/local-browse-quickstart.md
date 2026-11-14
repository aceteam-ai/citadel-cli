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
  and no telemetry. The co-browse browser's connections go through a guard
  inside Citadel that only connects to public internet addresses, and the
  browser's own background updates and syncing are turned off.

Citadel is source-available under the Elastic License 2.0, and free to use and
self-host. This page covers Windows 10 and 11 first. macOS and Linux are coming.

## What you need

- Windows 10 or 11 (x64).
- Microsoft Edge, which comes with Windows, or Google Chrome, version 109 or
  newer. `citadel.exe local doctor` checks this.
- One MCP-capable agent app. These are covered below:
  - the GitHub Copilot CLI (VS Code keeps its MCP servers in its own
    `mcp.json` file, which this guide does not cover);
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

The page also opens for any launch that gives `citadel.exe` a console of its
own, such as `start citadel` or `Start-Process citadel`, not only a double-click.

Keep the console window open while you use the page; closing it stops the page.
The steps below do the same things from a terminal.

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
can start again. Other commands:

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

## When something goes wrong

| What you see | What to do |
|---|---|
| "no supported browser found" | Install Microsoft Edge or Google Chrome. The message lists every place Citadel looked. |
| "organization policy blocks automation" | A work or school computer can forbid browser automation. Use a personal computer or a per-user Chrome install. |
| "exited during headless launch" | Run `& $citadel local doctor`. If it repeats, restart the computer. |
| The viewer says "No agent connected" | The agent app has not started Citadel. Check its config with `& $citadel local doctor`, then restart the app. |
| "another Citadel process already owns co-browse" | Another agent app is using co-browse. Close it, or use that one. |
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

Local mode does not need AceTeam. If you later run `citadel init` to connect
this computer to AceTeam, cloud agents still cannot reach the sessions your
local agent app starts. Your local approvals apply only to local agents, never
to cloud agents.
