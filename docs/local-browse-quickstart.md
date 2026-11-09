# Citadel local browse: quickstart

Citadel local mode lets an AI agent app on your computer drive a web browser on
the same computer, with you in charge:

- **You hand off and take back.** When the agent reaches a login or payment
  page, it hands the browser to you. You type into the page yourself in the
  Citadel viewer, then press **Hand back to agent**. The agent never sees what
  you type.
- **Approvals and an audit trail.** The agent can look at pages without
  asking, but it needs your approval in the viewer before it clicks, types or
  navigates. Every action is recorded on this computer, and
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
- the passphrase vault, which is only needed later to remember logins;
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

opencode's documentation lists `~/.config/opencode/opencode.json` (or
`opencode.jsonc`) and gives no Windows-specific user folder, so `local config`
marks that path as unverified on Windows. `--write` only edits plain JSON: a
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
   whether to allow this agent to click, type and navigate in this session.
   You can allow it for 24 hours, 8 hours or 1 hour, or deny it. You can
   revoke it at any time.
3. **Log in yourself.** At the login page the agent hands off. The viewer
   shows "Waiting for you: log in, then hand back". Press **Take control**,
   click into the page and type your password. Then press **Hand back to
   agent**. While you drive, the agent cannot see the screen or act.
4. **Watch the record.** The Activity panel lists every action. Your typed
   text never appears there, only how many characters the agent typed.
5. **Stop.** When the agent calls `local_cobrowse_stop`, or you press **End
   session**, the browser's cookies and logins for that session are deleted.

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
| "navigation refused" | The agent tried a local, private or non-web address. This is deliberate. |
| `rate_limited` | The agent is acting too fast. It should slow down. |
| The viewer link "expired or was already used" | Run `& $citadel local viewer`. |
| A sign-in popup or a second tab does not show | Popups and extra tabs are not shown in the viewer yet. Use a site's email-and-password login. |

## What it protects, and what it does not

Local mode protects against:

- the agent making mistakes;
- web pages trying to steer the agent (prompt injection), including toward
  the viewer;
- web pages in your own browser trying to reach Citadel's local ports;
- other user accounts on the same computer.

It does not protect against a hostile program running as **your** user
account, including an agent app with shell access that decides to misbehave.
Such a program can already read your files. This is the same boundary as any
local agent tool.

`& $citadel local audit --verify` checks that the audit log was not edited. That
detects tampering. It does not prove anything against a program running as
your account.

## Later: connect to AceTeam

Local mode does not need AceTeam. If you later run `citadel init` to connect
this computer to AceTeam, cloud agents still cannot reach the sessions your
local agent app starts. Your local approvals apply only to local agents, never
to cloud agents.
