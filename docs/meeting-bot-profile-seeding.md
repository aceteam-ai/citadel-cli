# Meeting Bot: Seeding the Persistent Chrome Profile

**Issue:** [#5122](https://github.com/aceteam-ai/citadel-cli/issues/5122) (epic [#5097](https://github.com/aceteam-ai/citadel-cli/issues/5097))

## Overview

The `MEETING_JOIN` handler (`internal/jobs/meeting_join.go`, [#5098](https://github.com/aceteam-ai/citadel-cli/issues/5098)) drives a headed Chromium (`internal/platform/meeting_browser.go`) into a Google Meet call. Early testing showed Google policy-rejects **anonymous** participants in many orgs — the bot needs a real, signed-in Google identity to be let in at all.

So the meeting browser's `--user-data-dir` is no longer a throwaway `os.MkdirTemp` profile: it is a **persistent** directory that a human signs into **once, by hand**, and every subsequent `MEETING_JOIN` job reuses the same cookies/session. This document covers the one-time (and periodic re-seed) manual steps. Everything else — resolving the profile path, creating it with locked-down permissions, reusing it across runs, and detecting when it has gone stale — is handled by the code; see `internal/platform/meeting_browser.go`.

**Why this can't be automated:** Google actively detects and blocks scripted/automated sign-in (CAPTCHA challenges, "this browser may not be secure" blocks, account lockouts). Do not attempt to script the login form. The seed step below is a real human, with a real mouse and keyboard, typing into a real browser window.

## What's code vs. what's manual

| Step | Owner |
|---|---|
| Create the `notetaker@aceteam.ai` Google account | Human (one-time, org admin) |
| Resolve/create the profile directory, lock permissions, reuse across runs | Code (`preparePersistentProfileDir` in `meeting_browser.go`) |
| Manual sign-in into that profile directory | Human (one-time, and periodically on cookie expiry) |
| Detect a signed-out profile and fail loudly instead of joining anonymously | Code (`IsGoogleSignInURL` + the check in `runJoinFlow`) |
| Live join verification against a real Meet call | Human |

## Where the profile lives

By default the profile directory is `<citadel ConfigDir>/meeting-profile` — the same node-local persistent-state root that already holds `tls/`, `logs/`, and `gateway/` (see `platform.ConfigDir()`; typically `~/.citadel-cli/meeting-profile`, or `/etc/citadel/meeting-profile` when citadel runs as root/system service).

Override it with the `CITADEL_MEETING_PROFILE_DIR` environment variable if the node's persistent state should live elsewhere (e.g. a dedicated data volume). The handler also exposes a `MeetingJoinHandler.ProfileDir` field for the worker's startup config to pin the same thing programmatically.

The directory is created (if missing) and `chmod 0700`'d on every `MeetingBrowser.Start()` — it holds real Google session cookies for the bot account, so it must never be group- or world-readable.

## One-time seeding procedure

1. **Create the bot account** (human, once): a dedicated Google account for the notetaker, e.g. `notetaker@aceteam.ai`. Use a strong, unique password and enroll it in 2FA per your org's policy — but see the note on 2FA below.

2. **Stop any running citadel worker on the target node** so nothing else launches Chrome against the same `--user-data-dir` while you're seeding it (Chrome locks a profile dir to one process; a concurrent launch will either fail to start or open a second, un-seeded window).

3. **Resolve the profile path** for the node you're seeding:

   ```bash
   # Matches platform.ConfigDir(); check CITADEL_MEETING_PROFILE_DIR first if the
   # node overrides it.
   echo "${CITADEL_MEETING_PROFILE_DIR:-$HOME/.citadel-cli/meeting-profile}"
   ```

   Create it if it doesn't exist yet, locked to owner-only:

   ```bash
   mkdir -p ~/.citadel-cli/meeting-profile
   chmod 700 ~/.citadel-cli/meeting-profile
   ```

4. **Launch a real, headed Chrome with that exact `--user-data-dir`** on the node (over VNC/physical display/remote desktop — this must be a real interactive session, not a job dispatch):

   ```bash
   google-chrome \
     --user-data-dir="$HOME/.citadel-cli/meeting-profile" \
     --no-first-run --no-default-browser-check \
     https://accounts.google.com/
   ```

5. **Sign in by hand** as `notetaker@aceteam.ai`. Complete any 2FA challenge manually.
   - If your org requires 2FA on this account, prefer a method that doesn't require a re-prompt on every session (e.g. "trust this device" / a long-lived device cookie) — Chrome persists that trust in the same profile directory, so a re-seed carries it forward. An OTP-app-only account with no trusted-device option will force a human back into this procedure far more often than intended.
   - Dismiss "Chrome sync" / "make Chrome your default browser" prompts; they don't matter for this profile.

6. **Verify the session sticks**: navigate to `https://meet.google.com/`. You should land signed in as the notetaker account (avatar/initial visible top-right), not on a sign-in page.

7. **Close Chrome normally** (not `kill -9`) so its session/cookie DB flushes cleanly to disk.

8. **Restart the citadel worker.** The next `MEETING_JOIN` job will launch Chromium against this same, now-seeded, profile directory and reuse the signed-in session.

## Cookie expiry and periodic re-seeding

Google session cookies are not permanent. Expect to redo steps 2–8 periodically (frequency depends on your org's session-length policy and whether 2FA trust was established) — or immediately if:

- The join flow starts failing with a **signed-out** error (see below) rather than a generic join-flow error.
- The account's password was rotated, or 2FA/device-trust was reset.
- The profile directory was deleted or moved (it is intentionally **never** auto-deleted by citadel — see "Why the profile survives" below).

## Signed-out detection

`internal/platform/meeting_browser.go` exports `IsGoogleSignInURL(url string) bool`, a deterministic check for whether the browser's current page is a Google sign-in redirect (`accounts.google.com`). `MeetingJoinHandler.runJoinFlow` (`internal/jobs/meeting_join.go`) calls this immediately after navigating to the meeting URL: if the persistent profile's session has expired, Meet redirects to sign-in, and the job fails **loudly** with a clear "profile is signed out, re-seed docs/meeting-bot-profile-seeding.md" error rather than silently limping on and joining anonymously (which is likely to be policy-rejected anyway, defeating the entire point of this profile).

A secondary, best-effort DOM signal (`meetAccountChipPresentJS` in `meeting_join.go`) checks for the signed-in account chip on the pre-join page; it is logged as a warning only (its selector is unverified against a live Meet call — see the file's `LIVE-TUNING REQUIRED` block) and does not fail the job on its own.

## Why the profile survives `Close()`

Prior to #5122, `MeetingBrowser` used `os.MkdirTemp` for its `--user-data-dir` and deleted it in `closeLocked()` after every meeting — appropriate for a throwaway automation profile, but it would have destroyed the seeded session after the very first meeting. `closeLocked()` now leaves the profile directory on disk untouched (only the in-memory handle is cleared); only the browser process and its dedicated Xvfb display are torn down per run.

## Concurrency note

Because the profile is shared and persistent, the bot account can be in **at most one meeting at a time per node** — Chrome refuses to open a second process against a locked `--user-data-dir`. This is an intentional constraint, not a bug: it mirrors the account itself (one Google session, one active call).
