// internal/jobs/meeting_join_teams.go
//
// Microsoft Teams pre-join flow for the MEETING_JOIN handler (issue #7000 —
// parallel to the Google Meet flow in meeting_join.go). It mirrors the Meet
// flow's SHAPE exactly — navigate, dismiss the "Continue on this browser"
// interstitial, enter a passcode (for /meet/<id>?p= links), type the guest
// display name, mute cam/mic, click "Join now", wait in the lobby for admission,
// then hand off to the SAME platform-agnostic record → transcribe path. Nothing
// past admission is Teams-specific.
//
// ⚠️ EVERY SELECTOR / JS SNIPPET BELOW IS UNVERIFIED (best-guess). Unlike the
// Meet flow — whose interstitial + host auto-admit path were confirmed against a
// live call on 2026-07-11 — the Teams web DOM here has NOT been exercised against
// a real Teams meeting. Teams renders its pre-join and in-call UI inside a heavy,
// frequently-changing Fluent UI app; the class names, data-tid values, and button
// labels WILL need live tuning against a real meeting before this can be trusted.
// The statically-verifiable parts (URL → platform detection, passcode extraction)
// live in meeting_join.go and are unit-tested; this file is the part a human must
// tune. Everything is kept in this one file so tuning is a single-file edit.
package jobs

import (
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Teams-specific lifecycle timeouts. Deliberately generous (mirrors the Meet
// flow) because Teams' web app is slow to hydrate: the pre-join screen commonly
// takes several seconds to render its controls after navigation, and the
// "Continue on this browser" interstitial can precede it.
const (
	// teamsPageSettle waits for the Teams web app shell to hydrate after
	// navigation before we start poking at the DOM. Teams is heavier than Meet;
	// give it a beat longer.
	teamsPageSettle = 7 * time.Second
	// teamsJoinButtonTimeout bounds the interstitial → passcode → name → mute →
	// join poll loop. Teams routes through an extra "Continue on this browser"
	// interstitial before the pre-join screen, so poll well past both renders.
	teamsJoinButtonTimeout = 60 * time.Second
)

// ---------------------------------------------------------------------------
// LIVE-TUNING REQUIRED — ALL UNVERIFIED (issue #7000)
//
// None of the selectors / JS below has been run against a real Teams meeting.
// They are best-guess authored from Teams' publicly-documented web-join UX
// (the "Continue on this browser" interstitial, a guest display-name field, the
// mic/camera pre-join toggles, "Join now", and the "someone will let you in"
// lobby). A human MUST open a real teams.microsoft.com meeting, drive this flow,
// and confirm/replace each constant. Kept together so tuning is one place.
//
//	verified against real Microsoft Teams on: NEVER — pending a live meeting
//
// ---------------------------------------------------------------------------
const (
	// teamsNameInputSelector: the guest "Enter name" / "Type your name" field on
	// the Teams web pre-join screen (anonymous join). UNVERIFIED best-guess:
	// Teams has historically used data-tid="prejoin-display-name-input"; the
	// aria/placeholder fallbacks hedge a rename.
	teamsNameInputSelector = `input[data-tid="prejoin-display-name-input"],input[placeholder*="name" i],input[aria-label*="name" i]`

	// teamsPasscodeInputSelector: the pre-join passcode field for a
	// /meet/<id>?p=<passcode> link. UNVERIFIED best-guess — Teams "meet" links
	// gate on a passcode entered on the pre-join screen. Placeholder/aria hedges.
	teamsPasscodeInputSelector = `input[data-tid*="passcode" i],input[placeholder*="passcode" i],input[aria-label*="passcode" i],input[type="password"]`

	// teamsIsAdmittedJS returns true once the in-call toolbar is present (the bot
	// has been admitted from the lobby / joined directly). UNVERIFIED best-guess:
	// Teams' hang-up/leave control has used data-tid="hangup-button"; the
	// aria-label "Leave" and a call-duration timer are corroborating fallbacks.
	teamsIsAdmittedJS = `(function(){` +
		`return !!document.querySelector('button[data-tid="hangup-button"],button[aria-label*="Leave" i],button[title*="Leave" i],[data-tid="call-duration"]');` +
		`})()`

	// teamsIsEndedJS returns true when the call ended or the bot was removed —
	// Teams swaps to a "You left the meeting" / "removed" / "meeting has ended"
	// screen. UNVERIFIED best-guess text scan (mirrors meetIsEndedJS).
	teamsIsEndedJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||"");` +
		`return /you (?:left|.?ve left) the meeting|meeting has ended|call ended|you.?ve been removed|removed you from the meeting|left the meeting/i.test(t);` +
		`})()`

	// teamsInLobbyJS returns true while the bot sits in the Teams lobby waiting
	// for a host to admit it ("Someone in the meeting should let you in soon").
	// UNVERIFIED best-guess text scan; used only for an informational log, never
	// fatal (the admission wait times out on its own).
	teamsInLobbyJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||"");` +
		`return /let you in soon|when the meeting starts|waiting for the host|in the lobby|someone.?s in the lobby/i.test(t);` +
		`})()`

	// teamsMuteMicCamJS best-effort turns OFF the mic and camera on the pre-join
	// screen so the bot joins muted and dark. UNVERIFIED best-guess: Teams toggle
	// buttons have used data-tid="toggle-mute" / "toggle-video" and expose an
	// aria-checked / aria-pressed state; we click only when the control reads as
	// currently ON so we don't accidentally re-enable it. Returns the count of
	// controls toggled (informational). Never throws on no-match.
	teamsMuteMicCamJS = `(function(){` +
		`var sels=['button[data-tid="toggle-mute"]','button[data-tid="toggle-video"]',` +
		`'button[aria-label*="microphone" i]','button[aria-label*="camera" i]'];` +
		`var n=0;` +
		`for(var i=0;i<sels.length;i++){var b=document.querySelector(sels[i]);if(!b)continue;` +
		`var on=(b.getAttribute('aria-pressed')==='true'||b.getAttribute('aria-checked')==='true');` +
		`if(on){b.click();n++;}}` +
		`return n;})()`

	// teamsLeaveJS clicks the in-call "Leave" / hang-up button so the bot exits
	// gracefully. UNVERIFIED best-guess; reuses the admitted-check selector.
	// Best-effort (browser Close() teardown is the backstop): returns true if a
	// button was clicked, false on no-match. Does NOT throw on no-match.
	teamsLeaveJS = `(function(){` +
		`var b=document.querySelector('button[data-tid="hangup-button"],button[aria-label*="Leave" i],button[title*="Leave" i]');` +
		`if(!b)return false;b.click();return true;})()`
)

// teamsContinueOnBrowserLabels are the button texts Teams shows on the "how do
// you want to join" interstitial to proceed in the current browser (rather than
// opening/installing the desktop app). Clicking one is best-effort — a Teams
// "meet" link sometimes lands straight on the pre-join screen with no
// interstitial. UNVERIFIED best-guess labels/casing.
var teamsContinueOnBrowserLabels = []string{
	"Continue on this browser",
	"Join on the web instead",
	"Use the web app instead",
	"Continue in this browser",
}

// teamsJoinButtonLabels are the visible button texts for the Teams pre-join
// join action, in priority order. UNVERIFIED best-guess casing/locale — confirm
// against a real call.
var teamsJoinButtonLabels = []string{"Join now", "Join", "Ask to join"}

// runTeamsJoinFlow drives the Microsoft Teams web pre-join sequence (issue
// #7000). It mirrors runMeetJoinFlow's structure: navigate, fail loudly if Teams
// forces a Microsoft sign-in (the bot joins anonymously by design), then poll
// interstitial → passcode → name → mute → join until admitted, then wait for the
// host to admit the bot from the lobby. Non-fatal steps log and continue; the
// join and admission are fatal. ⚠️ Every selector this touches is UNVERIFIED —
// see the LIVE-TUNING block above.
func (h *MeetingJoinHandler) runTeamsJoinFlow(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
	if err := br.Navigate(p.MeetingURL); err != nil {
		return fmt.Errorf("navigate to teams meeting url: %w", err)
	}
	time.Sleep(teamsPageSettle)

	// Fatal: the Teams meeting is refusing an anonymous/guest web join and has
	// redirected to a Microsoft sign-in wall (login.microsoftonline.com). The
	// sovereign bot has no MS profile by design, so fail loudly with an
	// actionable error rather than stalling at the login page. (The Meet flow's
	// analogous check is IsGoogleSignInURL / ErrMeetingBotSignedOut.)
	if curURL, err := br.CurrentURL(); err != nil {
		ctx.Log("warn", "     - could not read current URL for Teams sign-in check (non-fatal): %v", err)
	} else if platform.IsMicrosoftSignInURL(curURL) {
		return fmt.Errorf("teams meeting requires a Microsoft sign-in (redirected to %s) — this meeting does not allow anonymous/guest web join; the sovereign bot has no MS profile", curURL)
	}

	// Informational: note if we appear to be sitting in the Teams lobby already
	// (some meetings drop a guest straight into the lobby pre-join).
	if v, err := br.Evaluate(teamsInLobbyJS); err == nil {
		if inLobby, ok := v.(bool); ok && inLobby {
			ctx.Log("info", "     - Teams lobby detected on landing; waiting for host admission")
		}
	}

	// Fatal: poll admitted-check → continue-on-browser → passcode → name → mute
	// → join until the bot is in-call or the join button is clicked, or timeout.
	if err := pollForTeamsJoinClick(ctx, br, p.BotDisplayName, p.Passcode, teamsJoinButtonTimeout, meetingPollInterval); err != nil {
		return err
	}

	// Fatal: wait until admitted (in-call toolbar appears) or the lobby timeout.
	return h.waitUntilTeamsAdmitted(ctx, br, p)
}

// pollForTeamsJoinClick repeatedly (1) checks whether the bot is already in-call,
// (2) best-effort dismisses the "Continue on this browser" interstitial, (3)
// best-effort types the passcode (for /meet/<id>?p= links), (4) best-effort types
// the guest display name, (5) best-effort mutes mic/camera, and (6) tries the
// "Join now" button — until admitted or the button is clicked, or timeout. Steps
// 2–5 are non-fatal every pass (a control not rendered yet is normal); only
// reaching the timeout with neither admission nor a join click is fatal. Params
// mirror pollForJoinClick so tests can run it in milliseconds. ⚠️ UNVERIFIED
// selectors throughout.
func pollForTeamsJoinClick(ctx JobContext, page joinPage, botDisplayName, passcode string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Already admitted / in the call: success, no join button needed.
		if v, err := page.Evaluate(teamsIsAdmittedJS); err == nil {
			if b, ok := v.(bool); ok && b {
				ctx.Log("info", "     - already in Teams call, no join button needed")
				return nil
			}
		} else {
			ctx.Log("warn", "     - Teams already-admitted probe errored (non-fatal): %v", err)
		}

		// Best-effort: dismiss the "Continue on this browser" interstitial.
		if _, err := page.Evaluate(clickButtonByTextOptionalJS(teamsContinueOnBrowserLabels)); err != nil {
			ctx.Log("warn", "     - Teams continue-on-browser dismissal errored (non-fatal): %v", err)
		}

		// Best-effort: enter the pre-join passcode when the link carried one.
		if passcode != "" {
			if err := page.Type(teamsPasscodeInputSelector, passcode); err != nil {
				ctx.Log("warn", "     - could not enter Teams passcode (non-fatal, field may not be rendered yet): %v", err)
			}
		}

		// Best-effort: type the bot's guest display name.
		if err := page.Type(teamsNameInputSelector, botDisplayName); err != nil {
			ctx.Log("warn", "     - could not set Teams bot name (non-fatal, may not be rendered yet): %v", err)
		}

		// Best-effort: mute mic + camera before joining.
		if _, err := page.Evaluate(teamsMuteMicCamJS); err != nil {
			ctx.Log("warn", "     - Teams mute mic/cam errored (non-fatal): %v", err)
		}

		// Try the "Join now" button; a non-empty return means it was clicked.
		if v, err := page.Evaluate(clickButtonByTextOptionalJS(teamsJoinButtonLabels)); err != nil {
			ctx.Log("warn", "     - Teams join-button probe errored (retrying): %v", err)
		} else if label, ok := v.(string); ok && label != "" {
			ctx.Log("info", "     - clicked Teams join button (matched label %q)", label)
			return nil
		}

		time.Sleep(interval)
	}
	return fmt.Errorf("click Teams join button: no button matched labels %v within %s (interstitial/pre-join page may have changed — re-tune meeting_join_teams.go labels)", teamsJoinButtonLabels, timeout)
}

// waitUntilTeamsAdmitted polls the Teams admission heuristic until the bot is
// in-call or the lobby timeout elapses. Mirrors waitUntilAdmitted for Meet.
func (h *MeetingJoinHandler) waitUntilTeamsAdmitted(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
	deadline := time.Now().Add(admitTimeout)
	for time.Now().Before(deadline) {
		if v, err := br.Evaluate(teamsIsAdmittedJS); err == nil {
			if b, ok := v.(bool); ok && b {
				ctx.Log("info", "     - admitted to Teams meeting %s", p.MeetingID)
				return nil
			}
		} else {
			ctx.Log("warn", "     - Teams admission check errored (retrying): %v", err)
		}
		time.Sleep(meetingPollInterval)
	}
	return fmt.Errorf("not admitted to Teams meeting within %s (host did not let the bot in, or the admission selector is stale — re-tune teamsIsAdmittedJS)", admitTimeout)
}

// checkTeamsMeetingEnded runs the Teams end heuristic (the "you left / removed /
// meeting has ended" text scan). Best-guess (see the LIVE-TUNING block); a read
// error is treated as "not ended" so a transient DOM glitch never ends the call
// early. Called by checkMeetingEndedFor for the plain record→transcribe loop.
// Teams exposes no reliable participant-count selector we can trust yet, so —
// unlike Meet — there is no bot-alone signal here; end detection leans on the
// text scan plus the max_duration cap.
func checkTeamsMeetingEnded(page meetPage) (string, bool) {
	if v, err := page.Evaluate(teamsIsEndedJS); err == nil {
		if b, ok := v.(bool); ok && b {
			return "call_ended", true
		}
	}
	return "", false
}
