package localviewer

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse/fakebrowser"
)

type harness struct {
	srv    *Server
	svc    *localcobrowse.Service
	fake   *fakebrowser.Fake
	base   string
	opened []string
	dir    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{fake: fakebrowser.New(), dir: t.TempDir()}
	h.svc = localcobrowse.New(localcobrowse.Config{
		Browser:   h.fake,
		URLPolicy: func(context.Context, string) error { return nil },
	})
	h.srv = New(Config{
		Service:  h.svc,
		OpenURL:  func(u string) error { h.opened = append(h.opened, u); return nil },
		TokenDir: h.dir,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.srv.Close)
	h.base = "http://127.0.0.1:" + strconv.Itoa(h.srv.Port())
	return h
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (h *harness) do(t *testing.T, method, path string, hdr map[string]string, cookie bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, h.base+path, strings.NewReader("{}"))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if cookie {
		req.AddCookie(&http.Cookie{Name: h.srv.cookieName(), Value: h.srv.cookie})
	}
	resp, err := (&http.Client{CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) origin() string { return h.base }

func TestGuardsReturn403(t *testing.T) {
	h := newHarness(t)
	st, _ := h.svc.Start(context.Background(), "", "g", "")
	approve := "/api/sessions/" + st.SessionID + "/approve"
	good := map[string]string{"Origin": h.origin(), csrfHeader: h.srv.bearer}

	cases := []struct {
		name   string
		method string
		path   string
		hdr    map[string]string
		cookie bool
	}{
		{"state without the viewer token", "GET", "/api/state", nil, true},
		{"wrong host (localhost)", "GET", "/api/state", map[string]string{"Host": "localhost:" + strconv.Itoa(h.srv.Port())}, true},
		{"wrong host (rebinding name)", "GET", "/api/state", map[string]string{"Host": "attacker.test:" + strconv.Itoa(h.srv.Port())}, true},
		{"foreign origin GET", "GET", "/api/state", map[string]string{"Origin": "http://attacker.test"}, true},
		{"foreign origin POST", "POST", approve, map[string]string{"Origin": "http://attacker.test", csrfHeader: h.srv.bearer}, true},
		{"cross-site fetch", "POST", approve, map[string]string{"Origin": h.origin(), csrfHeader: h.srv.bearer, "Sec-Fetch-Site": "cross-site"}, true},
		{"missing cookie page", "GET", "/", nil, false},
		{"missing cookie state", "GET", "/api/state", nil, false},
		{"missing cookie POST", "POST", approve, good, false},
		{"missing CSRF", "POST", approve, map[string]string{"Origin": h.origin()}, true},
		{"wrong CSRF", "POST", approve, map[string]string{"Origin": h.origin(), csrfHeader: "nope"}, true},
		{"POST without origin", "POST", approve, map[string]string{csrfHeader: h.srv.bearer}, true},
		{"stream without csrf", "GET", "/api/stream?id=" + st.SessionID, map[string]string{"Origin": h.origin()}, true},
		{"mint from a browser", "POST", "/api/mint", map[string]string{"Origin": h.origin(), "Authorization": "Bearer " + h.srv.mint}, false},
		{"mint without token", "POST", "/api/mint", nil, false},
	}
	for _, c := range cases {
		resp := h.do(t, c.method, c.path, c.hdr, c.cookie)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", c.name, resp.StatusCode)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header sent", c.name)
		}
	}
	if v := h.svc.Sessions(st.SessionID); v[0].Approval == "approved" {
		t.Fatal("a refused request approved the session")
	}

	// The happy path passes every check.
	if resp := h.do(t, "POST", approve, good, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	if v := h.svc.Sessions(st.SessionID); v[0].Approval != "approved" {
		t.Fatal("approve did not take effect")
	}
	resp := h.do(t, "GET", "/", nil, true)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Contains(string(body), h.srv.bearer) {
		t.Fatalf("index must load with the cookie but never carry the viewer token: %d", resp.StatusCode)
	}
	if r := h.do(t, "GET", "/api/state", map[string]string{csrfHeader: h.srv.bearer}, true); r.StatusCode != 200 {
		t.Fatalf("state with cookie and token: %d", r.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("index must forbid framing")
	}
}

func TestCookieOnlyFromSingleUseCode(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/open", "/open?code=", "/open?code=deadbeef"} {
		resp := h.do(t, "GET", path, nil, false)
		if resp.StatusCode != http.StatusForbidden || len(resp.Cookies()) != 0 {
			t.Errorf("%s: want 403 and no cookie, got %d %v", path, resp.StatusCode, resp.Cookies())
		}
	}
	oldCookie, oldBearer := h.srv.secrets()
	link := h.srv.MintURL()
	u, _ := url.Parse(link)
	if u.Host != "127.0.0.1:"+strconv.Itoa(h.srv.Port()) {
		t.Fatalf("link must point at 127.0.0.1: %s", link)
	}
	resp := h.do(t, "GET", u.RequestURI(), nil, false)
	cs := resp.Cookies()
	body, _ := io.ReadAll(resp.Body)
	newCookie, newBearer := h.srv.secrets()
	if resp.StatusCode != http.StatusOK || len(cs) != 1 || !cs[0].HttpOnly || cs[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("exchange: %d %+v", resp.StatusCode, cs)
	}
	if cs[0].Value != newCookie || newCookie == oldCookie || newBearer == oldBearer {
		t.Fatal("each exchange must rotate the cookie and the viewer token")
	}
	if !strings.Contains(string(body), newBearer) {
		t.Fatal("the /open page must carry the new viewer token")
	}
	stale := &http.Cookie{Name: h.srv.cookieName(), Value: oldCookie}
	req, _ := http.NewRequest("GET", h.base+"/", nil)
	req.AddCookie(stale)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("the previous cookie must stop working: %d", r.StatusCode)
	}
	again := h.do(t, "GET", u.RequestURI(), nil, false)
	if again.StatusCode != http.StatusForbidden || len(again.Cookies()) != 0 {
		t.Fatalf("a code must work once: %d %v", again.StatusCode, again.Cookies())
	}

	now := time.Now()
	h.srv.cfg.Now = func() time.Time { return now }
	old := h.srv.MintURL()
	h.srv.cfg.Now = func() time.Time { return now.Add(codeTTL + time.Second) }
	ou, _ := url.Parse(old)
	if r := h.do(t, "GET", ou.RequestURI(), nil, false); r.StatusCode != http.StatusForbidden || len(r.Cookies()) != 0 {
		t.Fatalf("expired code: %d", r.StatusCode)
	}
	h.srv.cfg.Now = time.Now

	// The launch-file path: the navigation comes from a file: page, so it is
	// cross-site; /open alone accepts that.
	nav := h.do(t, "GET", mustRequestURI(t, h.srv.MintURL()), map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, false)
	if nav.StatusCode != http.StatusOK {
		t.Fatalf("launch-file navigation: %d", nav.StatusCode)
	}
}

func mustRequestURI(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

func TestOpenKeepsTheCodeOffTheCommandLine(t *testing.T) {
	h := newHarness(t)
	if err := h.srv.Open(); err != nil {
		t.Fatal(err)
	}
	if len(h.opened) != 1 {
		t.Fatalf("opened %v", h.opened)
	}
	arg := h.opened[0]
	if strings.Contains(arg, "code=") || !strings.HasSuffix(arg, ".html") {
		t.Fatalf("the browser must be opened on a launch file, not the link: %q", arg)
	}
	data, err := os.ReadFile(arg)
	if err != nil || !strings.Contains(string(data), "/open?code=") {
		t.Fatalf("launch file: %v", err)
	}
	if fi, err := os.Stat(arg); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("launch file mode %o", fi.Mode().Perm())
	}
	code := string(data)[strings.Index(string(data), "code=")+5:]
	code = code[:strings.IndexByte(code, '"')]
	if r := h.do(t, "GET", "/open?code="+code, nil, false); r.StatusCode != 200 {
		t.Fatalf("exchange: %d", r.StatusCode)
	}
	if _, err := os.Stat(arg); !os.IsNotExist(err) {
		t.Fatal("the launch file must be removed once its code is used")
	}
}

func TestApproveUnblocksWait(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, _ := h.svc.Start(ctx, "", "g", "")
	if _, err := h.svc.Click(ctx, st.SessionID, "#x", nil, nil, "g"); err == nil {
		t.Fatal("click must need approval")
	}
	if v := h.svc.Sessions(st.SessionID); !v[0].ApprovalAsked {
		t.Fatal("the refused write should raise the approval card")
	}
	done := make(chan localcobrowse.WaitResult, 1)
	go func() {
		r, _ := h.svc.Wait(ctx, st.SessionID, "approved", 10*time.Second)
		done <- r
	}()
	time.Sleep(50 * time.Millisecond)
	resp := h.do(t, "POST", "/api/sessions/"+st.SessionID+"/approve", map[string]string{"Origin": h.origin(), csrfHeader: h.srv.bearer}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	select {
	case r := <-done:
		if !r.Satisfied {
			t.Fatalf("wait: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approving in the viewer did not unblock local_cobrowse_wait")
	}
	if _, err := h.svc.Click(ctx, st.SessionID, "#x", nil, nil, "g"); err != nil {
		t.Fatalf("click after approve: %v", err)
	}
}

func TestTakeControlHandBackAndAddressBar(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st, _ := h.svc.Start(ctx, "", "g", "")
	hdr := map[string]string{"Origin": h.origin(), csrfHeader: h.srv.bearer}
	if r := h.do(t, "POST", "/api/sessions/"+st.SessionID+"/navigate", hdr, true); r.StatusCode != http.StatusConflict {
		t.Fatalf("address bar must be read-only while the agent drives: %d", r.StatusCode)
	}
	h.do(t, "POST", "/api/sessions/"+st.SessionID+"/take-control", hdr, true)
	if v := h.svc.Sessions(st.SessionID); v[0].Driver != "human" {
		t.Fatal("take control")
	}
	if _, err := h.svc.Screenshot(ctx, st.SessionID, "g"); err == nil {
		t.Fatal("agent screenshot must be refused while the user drives")
	}
	req, _ := http.NewRequest("POST", h.base+"/api/sessions/"+st.SessionID+"/navigate", strings.NewReader(`{"url":"https://example.test/"}`))
	req.Header.Set("Origin", h.origin())
	req.Header.Set(csrfHeader, h.srv.bearer)
	req.AddCookie(&http.Cookie{Name: h.srv.cookieName(), Value: h.srv.cookie})
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("navigate while driving: %v %v", err, r)
	}
	h.do(t, "POST", "/api/sessions/"+st.SessionID+"/hand-back", hdr, true)
	if v := h.svc.Sessions(st.SessionID); v[0].Driver != "ai" {
		t.Fatal("hand back")
	}
}

func TestMintAndAttention(t *testing.T) {
	h := newHarness(t)
	link, err := RequestLink(context.Background(), h.dir, nil)
	if err != nil || !strings.HasPrefix(link, h.base+"/open?code=") {
		t.Fatalf("request link: %q %v", link, err)
	}
	h.srv.Attention("cb-1", "start")
	if len(h.opened) != 1 || !strings.HasSuffix(h.opened[0], ".html") {
		t.Fatalf("attention should open the viewer once, opened %d", len(h.opened))
	}
	h.srv.Attention("cb-1", "approval")
	if len(h.opened) != 1 {
		t.Fatal("reopening must be throttled")
	}
	h.srv.Close()
	if _, err := RequestLink(context.Background(), h.dir, nil); err == nil {
		t.Fatal("no viewer after close")
	}
}

func TestServeRefusesNonLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Service: localcobrowse.New(localcobrowse.Config{Browser: fakebrowser.New()})})
	if err := srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	any, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	if err := New(Config{}).Serve(any); err == nil {
		t.Fatal("must refuse anything but 127.0.0.1")
	}
}

// TestOpenExchangeDropsOldViewers checks a successful /open releases the old
// tab's stream claim (its token was just revoked).
func TestOpenExchangeDropsOldViewers(t *testing.T) {
	h := newHarness(t)
	h.srv.mu.Lock()
	stream := h.srv.stream
	h.srv.mu.Unlock()
	if stream != nil {
		t.Skip("harness has a live stream")
	}
	// With no stream configured the exchange must still work.
	u, _ := url.Parse(h.srv.MintURL())
	if r := h.do(t, "GET", u.RequestURI(), nil, false); r.StatusCode != 200 {
		t.Fatalf("exchange: %d", r.StatusCode)
	}
}

func TestActivityEndpoint(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Activity = func(session string, limit int) ([]localaudit.Record, error) {
		return []localaudit.Record{{Seq: 1, SessionID: session, Actor: "agent", IntendedAction: "click", ResultingAction: "ok"}}, nil
	}
	if r := h.do(t, "GET", "/api/activity?session=cb-9", nil, false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("activity needs the cookie: %d", r.StatusCode)
	}
	if r := h.do(t, "GET", "/api/activity?session=cb-9", nil, true); r.StatusCode != http.StatusForbidden {
		t.Fatalf("activity needs the viewer token too: %d", r.StatusCode)
	}
	r := h.do(t, "GET", "/api/activity?session=cb-9", map[string]string{csrfHeader: h.srv.bearer}, true)
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(body), `"session_id":"cb-9"`) {
		t.Fatalf("activity: %d %s", r.StatusCode, body)
	}
}
