package platform

// teams_livetune_test.go — live DOM-tuning harness for the Teams join flow
// (citadel #660 / aceteam #7000). NOT a unit test: it is env-gated on
// TEAMS_LIVE_URL and drives the running meeting-service container's Chromium
// (published CDP port, default host 8208) against a REAL Teams meeting so a human
// can read off the true pre-join selectors that meeting_join_teams.go's constants
// only best-guess.
//
// Usage (sandbox OFF; localhost + the container must be reachable):
//
//	# ensure the citadel-meeting container has a live browser session:
//	curl -s -XPOST http://127.0.0.1:8207/sessions -H 'content-type: application/json' -d '{}'
//	# then run the harness against a live meeting URL:
//	TEAMS_LIVE_URL='https://teams.microsoft.com/meet/…?p=…' \
//	  OUT_DIR=/path/to/scratch \
//	  go test ./internal/platform -run TestTeamsLiveTune -v -timeout 20m -count=1
//
// It (1) PROBES whether the meeting even allows an anonymous/guest web join (a
// redirect to login.microsoftonline.com means selector tuning is a dead end and
// the fallback is the Graph-transcript MVP), (2) dumps an iframe- and
// shadow-DOM-aware inventory of every button/input/[role] on the pre-join screen
// to OUT_DIR, (3) screenshots the page, and (4) reports which of the current
// UNVERIFIED constants actually match live. Re-run after each edit to converge.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// teamsLiveCDPPort is the host published CDP port of the citadel-meeting
// container (compose maps 127.0.0.1:8208 -> container 9223 -> socat -> chrome).
// Override with TEAMS_CDP_PORT.
const teamsLiveDefaultCDPPort = 8208

// teamsLiveMeetingdURL is where meetingd's control API is published on the host
// (compose maps 127.0.0.1:8207 -> container 8102). Override with MEETINGD_URL.
const teamsLiveDefaultMeetingdURL = "http://127.0.0.1:8207"

// teamsInventoryJS walks the main document, every SAME-ORIGIN iframe (recursively),
// and every open shadow root, collecting the interactive controls that matter for
// a pre-join flow. It never throws (cross-origin iframe access is caught) so a
// partial DOM still yields a partial inventory. Returns a JSON string.
const teamsInventoryJS = `(function(){
  var out={url:location.href,title:document.title,frames:[],controls:[]};
  function pick(el,frame){
    try{
      var tag=(el.tagName||'').toLowerCase();
      var role=el.getAttribute&&el.getAttribute('role')||'';
      var isCtl = tag==='button'||tag==='input'||tag==='textarea'||tag==='a'||role==='button'||role==='textbox'||role==='checkbox';
      if(!isCtl) return;
      var txt=(el.innerText||el.value||'').trim().slice(0,80);
      out.controls.push({
        frame:frame, tag:tag, role:role,
        dataTid:el.getAttribute&&el.getAttribute('data-tid')||'',
        ariaLabel:el.getAttribute&&el.getAttribute('aria-label')||'',
        placeholder:el.getAttribute&&el.getAttribute('placeholder')||'',
        type:el.getAttribute&&el.getAttribute('type')||'',
        name:el.getAttribute&&el.getAttribute('name')||'',
        id:el.id||'',
        text:txt,
        disabled: (el.disabled===true)|| (el.getAttribute&&el.getAttribute('aria-disabled')==='true'),
        ariaPressed:el.getAttribute&&el.getAttribute('aria-pressed')||'',
        ariaChecked:el.getAttribute&&el.getAttribute('aria-checked')||''
      });
    }catch(e){}
  }
  function walk(root,frame){
    try{
      var all=root.querySelectorAll('*');
      for(var i=0;i<all.length;i++){
        var el=all[i];
        pick(el,frame);
        if(el.shadowRoot){ walk(el.shadowRoot, frame+'>shadow'); }
      }
    }catch(e){}
  }
  walk(document,'main');
  var ifr=document.querySelectorAll('iframe');
  for(var j=0;j<ifr.length;j++){
    var f=ifr[j]; var src=f.getAttribute('src')||''; var ok=false;
    try{ var d=f.contentDocument; if(d){ ok=true; walk(d,'iframe['+j+']'); } }catch(e){}
    out.frames.push({index:j,src:src,sameOriginAccessible:ok});
  }
  var bodyText=(document.body&&document.body.innerText||'').slice(0,600);
  out.bodyTextHead=bodyText;
  return JSON.stringify(out);
})()`

// teamsCandidateSelectors are the CURRENT best-guess selectors from
// meeting_join_teams.go (kept in sync by hand). The harness reports how many
// elements each matches live, so a 0 means "re-tune this constant".
var teamsCandidateSelectors = map[string]string{
	"name_input":     `input[data-tid="prejoin-display-name-input"],input[placeholder*="name" i],input[aria-label*="name" i]`,
	"passcode_input": `input[data-tid*="passcode" i],input[placeholder*="passcode" i],input[aria-label*="passcode" i],input[type="password"]`,
	"admitted":       `button[data-tid="hangup-button"],button[aria-label*="Leave" i],button[title*="Leave" i],[data-tid="call-duration"]`,
	"mute_mic":       `button[data-tid="toggle-mute"],button[aria-label*="microphone" i]`,
	"mute_cam":       `button[data-tid="toggle-video"],button[aria-label*="camera" i]`,
}

func teamsSelectorProbeJS(sel string) string {
	b, _ := json.Marshal(sel)
	return fmt.Sprintf(`(function(){try{return document.querySelectorAll(%s).length;}catch(e){return -1;}})()`, b)
}

func TestTeamsLiveTune(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEAMS_LIVE_URL"))
	if url == "" {
		t.Skip("set TEAMS_LIVE_URL to a live Teams meeting URL to run the live-tuning harness")
	}
	port := teamsLiveDefaultCDPPort
	if p := os.Getenv("TEAMS_CDP_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	}
	_ = os.MkdirAll(outDir, 0o755)
	stamp := strings.NewReplacer(":", "", "-", "", ".", "").Replace(fmt.Sprintf("%d", time.Now().Unix()))

	// Ensure a browser session exists in the container (idempotent: 409 = already up).
	meetingd := teamsLiveDefaultMeetingdURL
	if m := os.Getenv("MEETINGD_URL"); m != "" {
		meetingd = m
	}
	resp, err := http.Post(meetingd+"/sessions", "application/json", strings.NewReader(`{"max_duration_seconds":3600}`))
	if err != nil {
		t.Logf("POST /sessions error (continuing; a session may already be up): %v", err)
	} else {
		t.Logf("POST /sessions -> %s", resp.Status)
		resp.Body.Close()
	}

	br := NewCDPBrowser(port)
	if err := br.Ready(25 * time.Second); err != nil {
		t.Fatalf("container CDP not ready on host port %d: %v", port, err)
	}
	defer br.Close()

	if err := br.Navigate(url); err != nil {
		t.Fatalf("navigate to teams url: %v", err)
	}
	t.Logf("navigated; waiting 9s for Teams web app to hydrate…")
	time.Sleep(9 * time.Second)

	// ---- STEP 1: the gate. Anonymous/guest join allowed, or sign-in wall? ----
	cur, err := br.CurrentURL()
	if err != nil {
		t.Fatalf("read current url: %v", err)
	}
	t.Logf("CURRENT URL after settle: %s", cur)
	if IsMicrosoftSignInURL(cur) {
		t.Fatalf("PROBE=DEAD-END: redirected to a Microsoft sign-in wall (%s). This meeting refuses anonymous/guest web join; selector tuning cannot proceed — fallback is the Graph-transcript MVP.", cur)
	}
	t.Logf("PROBE=OK: not on a Microsoft sign-in wall; anonymous web join appears permitted so far.")

	// ---- STEP 2: inventory (iframe + shadow-DOM aware) ----
	invRaw, err := br.Evaluate(teamsInventoryJS)
	if err != nil {
		t.Fatalf("inventory evaluate: %v", err)
	}
	invStr, _ := invRaw.(string)
	invPath := filepath.Join(outDir, "teams_inventory_"+stamp+".json")
	if err := os.WriteFile(invPath, []byte(prettyJSON(invStr)), 0o644); err != nil {
		t.Logf("write inventory: %v", err)
	}
	t.Logf("wrote DOM inventory -> %s (%d bytes)", invPath, len(invStr))

	// ---- STEP 3: screenshot ----
	shot, err := cdpCommandPublished(port, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		t.Logf("screenshot cdp error: %v", err)
	} else if data, ok := shot["result"].(map[string]any); ok {
		if b64, ok := data["data"].(string); ok {
			if raw, derr := base64.StdEncoding.DecodeString(b64); derr == nil {
				shotPath := filepath.Join(outDir, "teams_prejoin_"+stamp+".png")
				_ = os.WriteFile(shotPath, raw, 0o644)
				t.Logf("wrote screenshot -> %s (%d bytes)", shotPath, len(raw))
			}
		}
	}

	// ---- STEP 4: which current UNVERIFIED constants match live? ----
	t.Logf("---- current best-guess selector match counts (0 = re-tune) ----")
	for name, sel := range teamsCandidateSelectors {
		v, err := br.Evaluate(teamsSelectorProbeJS(sel))
		if err != nil {
			t.Logf("  %-14s ERROR %v", name, err)
			continue
		}
		t.Logf("  %-14s matches=%v", name, v)
	}
	t.Logf("---- done. Read %s + the .png, then patch meeting_join_teams.go constants. ----", invPath)
}

// TestTeamsLiveJoin drives the verified pre-join controls (name → audio mode →
// Join now) against the live meeting, then re-inventories the resulting screen
// (lobby or in-call) so the in-call/lobby/leave selectors — unreadable until the
// bot is past pre-join — can be tuned. Env-gated: needs BOTH TEAMS_LIVE_URL and
// TEAMS_DO_JOIN=1 (so a plain harness run never actually joins). Reuses the
// container session left by TestTeamsLiveTune (or creates one).
func TestTeamsLiveJoin(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEAMS_LIVE_URL"))
	if url == "" || os.Getenv("TEAMS_DO_JOIN") != "1" {
		t.Skip("set TEAMS_LIVE_URL and TEAMS_DO_JOIN=1 to actually join the meeting")
	}
	port := teamsLiveDefaultCDPPort
	if p := os.Getenv("TEAMS_CDP_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	}
	_ = os.MkdirAll(outDir, 0o755)
	botName := os.Getenv("BOT_NAME")
	if botName == "" {
		botName = "AceTeam Notetaker"
	}

	meetingd := teamsLiveDefaultMeetingdURL
	if m := os.Getenv("MEETINGD_URL"); m != "" {
		meetingd = m
	}
	if resp, err := http.Post(meetingd+"/sessions", "application/json", strings.NewReader(`{"max_duration_seconds":3600}`)); err == nil {
		t.Logf("POST /sessions -> %s", resp.Status)
		resp.Body.Close()
	}

	br := NewCDPBrowser(port)
	if err := br.Ready(25 * time.Second); err != nil {
		t.Fatalf("container CDP not ready on host port %d: %v", port, err)
	}
	defer br.Close()

	if err := br.Navigate(url); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	time.Sleep(9 * time.Second)
	if cur, _ := br.CurrentURL(); IsMicrosoftSignInURL(cur) {
		t.Fatalf("sign-in wall at %s", cur)
	}

	// Fill the guest display name (verified selector).
	if err := br.Type(`input[data-tid="prejoin-display-name-input"]`, botName); err != nil {
		t.Logf("set name (non-fatal): %v", err)
	} else {
		t.Logf("set bot name = %q", botName)
	}

	// Best-effort: select "Computer audio" so meeting audio routes to the
	// recording sink (a capture bot must hear the room). Non-fatal if the radio
	// is absent (device-less container may force the no-audio nudge).
	if _, err := br.Evaluate(`(function(){var r=document.querySelector('input[name="radiogroup-ra"][aria-label="Computer audio"],#radio-rb');if(r){r.click();return true;}return false;})()`); err != nil {
		t.Logf("select Computer audio (non-fatal): %v", err)
	}

	// Click "Join now" by verified data-tid.
	if _, err := br.Evaluate(`(function(){var b=document.querySelector('button[data-tid="prejoin-join-button"]');if(!b)throw new Error("prejoin-join-button not found");b.click();return true;})()`); err != nil {
		t.Fatalf("click Join now: %v", err)
	}
	t.Logf("clicked Join now; waiting 15s to land in lobby or in-call…")
	time.Sleep(15 * time.Second)

	// Dismiss a possible "Continue without audio or video" confirmation.
	if _, err := br.Evaluate(`(function(){var bs=document.querySelectorAll('button');for(var i=0;i<bs.length;i++){if(/continue without audio/i.test(bs[i].innerText||'')){bs[i].click();return true;}}return false;})()`); err == nil {
		time.Sleep(4 * time.Second)
	}

	stamp := fmt.Sprintf("join_%d", time.Now().Unix())
	invRaw, err := br.Evaluate(teamsInventoryJS)
	if err != nil {
		t.Fatalf("post-join inventory: %v", err)
	}
	invStr, _ := invRaw.(string)
	invPath := filepath.Join(outDir, "teams_inventory_"+stamp+".json")
	_ = os.WriteFile(invPath, []byte(prettyJSON(invStr)), 0o644)
	t.Logf("wrote post-join inventory -> %s", invPath)

	// Screenshot (log raw keys if the shape is unexpected).
	if shot, err := cdpCommandPublished(port, "Page.captureScreenshot", map[string]any{"format": "png"}); err != nil {
		t.Logf("screenshot error: %v", err)
	} else if res, ok := shot["result"].(map[string]any); ok {
		if b64, ok := res["data"].(string); ok {
			if raw, derr := base64.StdEncoding.DecodeString(b64); derr == nil {
				_ = os.WriteFile(filepath.Join(outDir, "teams_"+stamp+".png"), raw, 0o644)
				t.Logf("wrote screenshot -> teams_%s.png (%d bytes)", stamp, len(raw))
			}
		}
	} else {
		t.Logf("screenshot unexpected response keys: %v", keysOf(shot))
	}

	// Report current in-call/lobby selector matches.
	inCall := map[string]string{
		"admitted":  `button[data-tid="hangup-button"],button[aria-label*="Leave" i],button[title*="Leave" i],[data-tid="call-duration"]`,
		"leave_btn": `button[data-tid="hangup-button"],button[aria-label*="Leave" i]`,
	}
	for name, sel := range inCall {
		if v, err := br.Evaluate(teamsSelectorProbeJS(sel)); err == nil {
			t.Logf("  %-10s matches=%v", name, v)
		}
	}
	if v, err := br.Evaluate(`(function(){return (document.body&&document.body.innerText||'').slice(0,500);})()`); err == nil {
		t.Logf("post-join body text head: %v", v)
	}
}

// TestTeamsCapture attaches to the ALREADY-RUNNING container browser WITHOUT
// navigating (so it does not knock the bot out of the lobby/call) and dumps the
// current DOM inventory + screenshot. Run it repeatedly: once after the host
// admits the bot it captures the true in-call toolbar (leave/hangup/duration)
// selectors. Gated on TEAMS_CAPTURE=1.
func TestTeamsCapture(t *testing.T) {
	if os.Getenv("TEAMS_CAPTURE") != "1" {
		t.Skip("set TEAMS_CAPTURE=1 to snapshot the current (already-navigated) browser state")
	}
	port := teamsLiveDefaultCDPPort
	if p := os.Getenv("TEAMS_CDP_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	}
	br := NewCDPBrowser(port)
	if err := br.Ready(15 * time.Second); err != nil {
		t.Fatalf("CDP not ready: %v", err)
	}
	defer br.Close()

	cur, _ := br.CurrentURL()
	t.Logf("current url: %s", cur)
	stamp := fmt.Sprintf("cap_%d", time.Now().Unix())
	invRaw, err := br.Evaluate(teamsInventoryJS)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	invStr, _ := invRaw.(string)
	invPath := filepath.Join(outDir, "teams_inventory_"+stamp+".json")
	_ = os.WriteFile(invPath, []byte(prettyJSON(invStr)), 0o644)
	t.Logf("wrote inventory -> %s", invPath)
	saveScreenshot(t, port, filepath.Join(outDir, "teams_"+stamp+".png"))
	if v, err := br.Evaluate(`(function(){return (document.body&&document.body.innerText||'').slice(0,500);})()`); err == nil {
		t.Logf("body text head: %v", v)
	}
}

// saveScreenshot writes a PNG via Page.captureScreenshot. cdpCommandPublished
// returns the CDP result object directly (base64 PNG under "data"), so read
// shot["data"] — NOT shot["result"]["data"].
func saveScreenshot(t *testing.T, port int, path string) {
	shot, err := cdpCommandPublished(port, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		t.Logf("screenshot error: %v", err)
		return
	}
	b64, ok := shot["data"].(string)
	if !ok {
		if res, ok := shot["result"].(map[string]any); ok {
			b64, _ = res["data"].(string)
		}
	}
	if b64 == "" {
		t.Logf("screenshot: no data in response keys %v", keysOf(shot))
		return
	}
	raw, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		t.Logf("screenshot decode: %v", derr)
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
	t.Logf("wrote screenshot -> %s (%d bytes)", path, len(raw))
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// prettyJSON best-effort indents a JSON string for human reading; returns the
// input unchanged if it does not parse.
func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}
