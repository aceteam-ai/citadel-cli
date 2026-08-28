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
// The selectors / JS snippets below were VERIFIED against a real, live Teams
// meeting on 2026-08-01 (light-meetings anonymous web join, driven through the
// meeting-service container on node 1297) — see the VERIFIED block below for the
// captured DOM shapes and exactly what was and was not exercised (e.g. the
// mic/cam-off toggles could not be tested on a device-less container). Teams
// renders its pre-join and in-call UI inside a heavy, frequently-changing Fluent
// UI app, so a future Teams UI change can still drift these constants — if the
// join flow starts failing, re-run the live-tuning harness
// (internal/platform/teams_livetune_test.go) against a real meeting rather than
// assuming they are still correct. The statically-verifiable parts (URL →
// platform detection, passcode extraction) live in meeting_join.go and are
// unit-tested; this file is the part a human must tune. Everything is kept in
// this one file so tuning is a single-file edit.
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
// VERIFIED against a live Teams meeting (issue #7000 / citadel #660)
//
// The selectors / JS below were tuned against a REAL teams.microsoft.com meeting
// driven through the meeting-service container on node 1297. An anonymous /meet/
// <id>?p=<passcode> link redirects Teams to its "light meetings" web experience
// (…/light-meetings/launch?…&anon=true&lightExperience=true) whose pre-join and
// in-call DOM is stable and data-tid/id-addressable (no iframes, no shadow DOM).
// The captured DOM: pre-join name field data-tid="prejoin-display-name-input",
// join button data-tid="prejoin-join-button", audio radios aria-label "Computer
// audio"/"Don't use audio"; lobby text "Someone will let you in shortly."; and
// in-call toolbar ids #hangup-button (Leave), #roster-button (People),
// #screenshare-button (Share), plus chat composer div[data-tid="ckeditor"].
//
//	verified against real Microsoft Teams on: 2026-08-01 (light-meetings anon web join)
//
// NOTE: verified on the device-LESS container (no mic/camera → the pre-join
// mic/cam toggles are disabled and the bot "just listens in", which is correct
// for a capture bot). The mic/cam-off constants below are kept for a
// device-equipped node but could not be exercised here.
// ---------------------------------------------------------------------------
const (
	// teamsNameInputSelector: the guest "Type your name" field on the Teams web
	// pre-join screen (anonymous join). VERIFIED: data-tid="prejoin-display-name-
	// input" (placeholder "Type your name"). The aria/placeholder fallbacks hedge
	// a future rename.
	teamsNameInputSelector = `input[data-tid="prejoin-display-name-input"],input[placeholder*="name" i],input[aria-label*="name" i]`

	// teamsPasscodeInputSelector: the pre-join passcode field for a
	// /meet/<id>?p=<passcode> link. NOTE: in the verified light-meetings anon flow
	// the passcode rides in the URL (?p=…) and NO passcode field renders on the
	// pre-join screen, so the passcode-typing step is a harmless no-op there. Kept
	// as best-effort for meeting shapes that do gate on a typed passcode.
	teamsPasscodeInputSelector = `input[data-tid*="passcode" i],input[placeholder*="passcode" i],input[aria-label*="passcode" i],input[type="password"]`

	// teamsIsAdmittedJS returns true once the in-call toolbar is present (the bot
	// has been admitted from the lobby / joined directly). VERIFIED: the light-
	// meetings in-call toolbar exposes id="hangup-button" (the Leave control — it
	// carries NEITHER a data-tid NOR an aria-label, only visible text "Leave", so
	// it is addressed by id), plus id="roster-button" (People), id="screenshare-
	// button" (Share), and the chat composer div[data-tid="ckeditor"]. Any of
	// these is an unambiguous in-call marker (none render pre-join / in-lobby).
	teamsIsAdmittedJS = `(function(){` +
		`return !!document.querySelector('#hangup-button,#roster-button,#screenshare-button,div[data-tid="ckeditor"]');` +
		`})()`

	// teamsIsEndedJS returns true when the call ended or the bot was removed —
	// Teams swaps to a "You left the meeting" / "removed" / "meeting has ended"
	// screen. UNVERIFIED best-guess text scan (mirrors meetIsEndedJS).
	teamsIsEndedJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||"");` +
		`return /you (?:left|.?ve left) the meeting|meeting has ended|call ended|you.?ve been removed|removed you from the meeting|left the meeting/i.test(t);` +
		`})()`

	// teamsInLobbyJS returns true while the bot sits in the Teams lobby waiting
	// for a host to admit it. VERIFIED: the light-meetings lobby greets the guest
	// with "Hi, <name>. Someone will let you in shortly." — note "shortly", NOT
	// the "soon" the pre-verification guess assumed. Informational only, never
	// fatal (the admission wait times out on its own).
	teamsInLobbyJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||"");` +
		`return /let you in (?:soon|shortly)|will let you in|when the meeting starts|waiting for the host|in the lobby|someone.?s in the lobby/i.test(t);` +
		`})()`

	// teamsMuteMicCamJS best-effort turns OFF the mic and camera on the pre-join
	// screen so the bot joins muted and dark. VERIFIED element shapes: the toggles
	// are <input role="switch" type="checkbox" data-tid="toggle-mute"> and
	// data-tid="toggle-video" (NOT <button> as first guessed) — so query the input
	// forms. On the device-less container both are DISABLED (nothing to unmute) so
	// this is a no-op there; on a device-equipped node they default ON and we click
	// to turn them off. State reads from .checked / aria-checked; we click only
	// when currently ON and enabled, so we never accidentally re-enable. Returns
	// the count toggled (informational). Never throws on no-match.
	teamsMuteMicCamJS = `(function(){` +
		`var sels=['input[data-tid="toggle-mute"]','input[data-tid="toggle-video"]',` +
		`'button[data-tid="toggle-mute"]','button[data-tid="toggle-video"]'];` +
		`var n=0;` +
		`for(var i=0;i<sels.length;i++){var b=document.querySelector(sels[i]);if(!b)continue;` +
		`if(b.disabled)continue;` +
		`var on=(b.checked===true||b.getAttribute('aria-checked')==='true'||b.getAttribute('aria-pressed')==='true');` +
		`if(on){b.click();n++;}}` +
		`return n;})()`

	// teamsLeaveJS clicks the in-call "Leave" / hang-up button so the bot exits
	// gracefully. VERIFIED: the control is id="hangup-button" (no data-tid, no
	// aria-label — only visible text "Leave"), so we target the id first and fall
	// back to a button whose trimmed text is exactly "Leave". Best-effort (browser
	// Close() teardown is the backstop): returns true if a button was clicked,
	// false on no-match. Does NOT throw on no-match.
	teamsLeaveJS = `(function(){` +
		`var b=document.querySelector('#hangup-button');` +
		`if(!b){var bs=document.querySelectorAll('button');` +
		`for(var i=0;i<bs.length;i++){if((bs[i].innerText||"").trim()==="Leave"){b=bs[i];break;}}}` +
		`if(!b)return false;b.click();return true;})()`
)

// teamsContinueOnBrowserLabels are the button texts Teams shows on the "how do
// you want to join" interstitial to proceed in the current browser (rather than
// opening/installing the desktop app). Clicking one is best-effort — the verified
// light-meetings anon link lands straight on the pre-join screen with NO such
// interstitial, but non-anon / desktop-preferring shapes still show it.
var teamsContinueOnBrowserLabels = []string{
	"Continue on this browser",
	"Join on the web instead",
	"Use the web app instead",
	"Continue in this browser",
}

// teamsJoinButtonLabels are the visible button texts for the Teams pre-join
// join action, in priority order. VERIFIED: the light-meetings join button reads
// "Join now" (and is also addressable by data-tid — see teamsClickJoinJS, tried
// first). The extra labels hedge locale / an "Ask to join" gated meeting.
var teamsJoinButtonLabels = []string{"Join now", "Join", "Ask to join"}

// teamsClickJoinJS clicks the pre-join join control by its VERIFIED stable
// data-tid (button[data-tid="prejoin-join-button"], id="prejoin-join-button"),
// returning true on click. Tried before the text-label fallback because the
// data-tid is locale-independent and unambiguous (the sibling Cancel control is
// data-tid="prejoin-cancel-button"). Never throws on no-match.
const teamsClickJoinJS = `(function(){` +
	`var b=document.querySelector('button[data-tid="prejoin-join-button"]');` +
	`if(!b)return false;b.click();return true;})()`

// teamsSelectComputerAudioJS selects the "Computer audio" pre-join radio so the
// meeting's audio routes to the browser (and thus into the recording sink) — a
// capture bot MUST hear the room. VERIFIED: the audio mode is a radio group whose
// options carry aria-label "Computer audio" / "Don't use audio" (the dynamic
// name/id suffixes are NOT stable, so match on aria-label). Best-effort: on the
// device-less container Teams may still fall back to "just listening in"; clicking
// only when not already checked. Never throws on no-match.
const teamsSelectComputerAudioJS = `(function(){` +
	`var r=document.querySelector('input[type="radio"][aria-label="Computer audio" i]');` +
	`if(r&&!r.checked){r.click();return true;}return false;})()`

// teamsContinueWithoutAudioLabels dismiss the "Are you sure you don't want audio
// or video?" confirmation Teams raises after "Join now" when no capture device is
// present (the verified device-less container case). Clicking "Continue without
// audio or video" lets the bot proceed into the lobby/call to listen-and-record.
var teamsContinueWithoutAudioLabels = []string{
	"Continue without audio or video",
	"Continue without audio",
}

// runTeamsJoinFlow drives the Microsoft Teams web pre-join sequence (issue
// #7000). It mirrors runMeetJoinFlow's structure: navigate, fail loudly if Teams
// forces a Microsoft sign-in (the bot joins anonymously by design), then poll
// interstitial → passcode → name → mute → join until admitted, then wait for the
// host to admit the bot from the lobby. Non-fatal steps log and continue; the
// join and admission are fatal. Selectors VERIFIED against a live light-meetings
// anon join (2026-08-01) — see the VERIFIED block above.
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
// mirror pollForJoinClick so tests can run it in milliseconds. Selectors VERIFIED
// against a live light-meetings anon join (2026-08-01).
//
// Also selects on ctx.Context().Done(), mirroring pollForJoinClick's Meet
// counterpart (citadel#488): this loop runs before waitUntilTeamsAdmitted in
// runTeamsJoinFlow, so a shutdown/drain landing during the pre-join sequence
// needs the same prompt-return contract.
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

		// Best-effort: select "Computer audio" so the meeting's audio routes into
		// the browser (and thus the recording sink) — a capture bot must hear the
		// room. No-op if the radio is absent or already selected.
		if _, err := page.Evaluate(teamsSelectComputerAudioJS); err != nil {
			ctx.Log("warn", "     - Teams select-computer-audio errored (non-fatal): %v", err)
		}

		// Try the join button by VERIFIED data-tid first, then fall back to the
		// visible-text labels; a non-empty/true return means it was clicked.
		if v, err := page.Evaluate(teamsClickJoinJS); err == nil {
			if clicked, ok := v.(bool); ok && clicked {
				ctx.Log("info", "     - clicked Teams join button (data-tid=prejoin-join-button)")
				return nil
			}
		} else {
			ctx.Log("warn", "     - Teams join-by-tid probe errored (falling back to labels): %v", err)
		}
		if v, err := page.Evaluate(clickButtonByTextOptionalJS(teamsJoinButtonLabels)); err != nil {
			ctx.Log("warn", "     - Teams join-button probe errored (retrying): %v", err)
		} else if label, ok := v.(string); ok && label != "" {
			ctx.Log("info", "     - clicked Teams join button (matched label %q)", label)
			return nil
		}

		select {
		case <-ctx.Context().Done():
			return fmt.Errorf("Teams meeting cancelled while waiting for the join button: %w", ctx.Context().Err())
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("click Teams join button: no button matched labels %v within %s (interstitial/pre-join page may have changed — re-tune meeting_join_teams.go labels)", teamsJoinButtonLabels, timeout)
}

// waitUntilTeamsAdmitted polls the Teams admission heuristic until the bot is
// in-call, the lobby timeout elapses, or the job context is cancelled (worker
// shutdown/drain, citadel#488 — mirrors waitUntilAdmitted for Meet).
func (h *MeetingJoinHandler) waitUntilTeamsAdmitted(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
	deadline := time.Now().Add(admitTimeout)
	for time.Now().Before(deadline) {
		// Best-effort: dismiss the "Are you sure you don't want audio or video?"
		// confirmation Teams raises right after "Join now" on a device-less bot —
		// it can sit between the click and the lobby/call, so clear it each pass.
		if _, err := br.Evaluate(clickButtonByTextOptionalJS(teamsContinueWithoutAudioLabels)); err != nil {
			ctx.Log("warn", "     - Teams continue-without-audio dismissal errored (non-fatal): %v", err)
		}
		if v, err := br.Evaluate(teamsIsAdmittedJS); err == nil {
			if b, ok := v.(bool); ok && b {
				ctx.Log("info", "     - admitted to Teams meeting %s", p.MeetingID)
				return nil
			}
		} else {
			ctx.Log("warn", "     - Teams admission check errored (retrying): %v", err)
		}
		select {
		case <-ctx.Context().Done():
			err := fmt.Errorf("Teams meeting cancelled while waiting for admission: %w", ctx.Context().Err())
			ctx.Log("info", "     - admission wait for Teams meeting %s cancelled (shutdown/drain): %v", p.MeetingID, err)
			return err
		case <-time.After(meetingPollInterval):
		}
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
