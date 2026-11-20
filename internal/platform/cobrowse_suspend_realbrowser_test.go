package platform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

// The OS-specific opt-in wrappers supply a fresh private throwaway manager.
// This exercises the real capture/scroll/observation adapters, NOT a suspend
// action, restart, persistent profile, account binding or complete reopen.
// All pages are synthetic loopback documents without forms/storage/credentials.
func runSuspendPagesRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	m.SetLaunchOptions(LaunchOptions{Quiet: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, `<!doctype html><title>suspend fixture</title>
<style>html{scroll-behavior:auto}body{margin:0;width:10000px;height:10000px}</style>
<p>synthetic scroll surface</p>`)
	}))
	t.Cleanup(srv.Close)
	urls := []string{srv.URL + "/page?variant=one#position", srv.URL + "/page?variant=one#position", srv.URL + "/page?variant=two#position"}
	st, err := m.StartSession(urls[0])
	if err != nil {
		t.Fatal("start private fixture browser", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	m.mu.Lock()
	s := m.sessions[st.ID]
	m.mu.Unlock()
	if s == nil {
		t.Fatal("private fixture session disappeared")
	}
	s.mu.Lock()
	proc, generation := s.proc, s.generation
	s.mu.Unlock()
	if proc == nil || proc.browserID == "" || generation == 0 {
		t.Fatal("private fixture lacks a browser incarnation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// A local fixture-only incarnation check. It does not substitute for the
	// future public manager's authenticated admission or lifecycle transaction.
	fence := func() error {
		s.mu.Lock()
		current := s.proc == proc && s.generation == generation
		s.mu.Unlock()
		select {
		case <-proc.exited:
			current = false
		default:
		}
		if !current || ctx.Err() != nil {
			return errors.New("private fixture incarnation changed")
		}
		return nil
	}
	source := browserSuspendPageSource()
	if err := source.verify(ctx, proc.debugPort, proc.browserID); err != nil {
		t.Fatal("verify private fixture browser", err)
	}
	ws, err := fetchCDPBrowserWSContext(ctx, proc.debugPort)
	if err != nil || ws != "ws://127.0.0.1:"+strconv.Itoa(proc.debugPort)+"/devtools/browser/"+proc.browserID {
		t.Fatal("fixture browser debugger endpoint was not the launched incarnation")
	}
	browser, err := openSuspendCaptureConn(ctx, pin{port: proc.debugPort, ws: ws})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	browserCall := func(method string, params map[string]any) map[string]any {
		t.Helper()
		if fence() != nil {
			t.Fatal("fixture browser changed before control call")
		}
		out, err := browser.Call(method, params)
		if err != nil || fence() != nil {
			t.Fatal("private fixture control call failed", err)
		}
		return out
	}
	list := func(ctx context.Context) ([]cdpdiscovery.Target, error) {
		return source.list(ctx, proc.debugPort)
	}
	var initial []cdpdiscovery.Target
	awaitSuspendFixture(t, ctx, "initial document", func(ctx context.Context) bool {
		initial, err = list(ctx)
		return err == nil && len(initial) == 1 && initial[0].URL == urls[0]
	})
	ids := []string{initial[0].ID}
	create := func(url string) string {
		t.Helper()
		out := browserCall("Target.createTarget", map[string]any{"url": url, "background": true})
		id, _ := out["targetId"].(string)
		if !validSuspendRestoredTargetID(id) {
			t.Fatal("fixture browser returned an invalid new target")
		}
		return id
	}
	ids = append(ids, create(urls[1]), create(urls[2]))
	page := func(id string) pin {
		return pin{background: true, port: proc.debugPort, id: id,
			ws: "ws://127.0.0.1:" + strconv.Itoa(proc.debugPort) + "/devtools/page/" + id}
	}
	withPage := func(ctx context.Context, id string, run func(*cdpConn) error) error {
		c, err := openSuspendCaptureConn(ctx, page(id))
		if err != nil {
			return err
		}
		defer c.Close()
		return run(c)
	}
	restore := func(id string, tab suspendTab) {
		t.Helper()
		awaitSuspendFixture(t, ctx, "loaded scrollable document", func(ctx context.Context) bool {
			return withPage(ctx, id, func(c *cdpConn) error { return restoreSuspendTabScrollOn(ctx, c, tab, fence) }) == nil
		})
	}
	read := func(id string) suspendTab {
		t.Helper()
		tab, err := source.read(ctx, page(id), fence)
		if err != nil {
			t.Fatal("read private fixture tab", err)
		}
		return tab
	}
	manifest := suspendManifest{Version: suspendManifestVersion, SessionID: st.ID, Generation: generation}
	for i, id := range ids {
		wanted := suspendTab{URL: urls[i], ScrollX: float64(128 + 73*i), ScrollY: float64(512 + 129*i)}
		restore(id, wanted)
		captured := read(id)
		if captured.URL != wanted.URL || captured.Title != "suspend fixture" ||
			math.Abs(captured.ScrollX-wanted.ScrollX) > 1 || math.Abs(captured.ScrollY-wanted.ScrollY) > 1 {
			t.Fatal("real page capture did not retain the requested URL, title and both scroll axes")
		}
		captured.Active = i == 2
		manifest.Tabs = append(manifest.Tabs, captured)
	}
	browserCall("Target.activateTarget", map[string]any{"targetId": ids[2]})
	// The page activation above is real, but this local selection callback
	// exercises the verifier contract, not future manager target tracking.
	selected := ids[2]
	active := func() (string, error) { return selected, nil }
	plan := suspendRestoredPagePlan{manifest: manifest, port: proc.debugPort, browserID: proc.browserID, targetIDs: ids}
	verify := func() error { return verifySuspendRestoredPages(ctx, plan, active, fence) }
	if err := verify(); err != nil {
		t.Fatal("complete real page observations failed", err)
	}
	t.Log("captured and observed three real tabs, including duplicate URLs")

	// Reset independently of the production restore helper. A final observer
	// must refuse, and must leave all pages at zero rather than repairing them.
	for _, id := range ids {
		if err := withPage(ctx, id, func(c *cdpConn) error {
			_, err := evaluateOn(c, `window.scrollTo({left:0,top:0,behavior:"instant"}); true`, false)
			return err
		}); err != nil {
			t.Fatal("reset private fixture scroll", err)
		}
	}
	if err := verify(); err == nil {
		t.Fatal("final observation acknowledged unrestored pages")
	}
	for _, id := range ids {
		got := read(id)
		if got.ScrollX != 0 || got.ScrollY != 0 {
			t.Fatal("read-only observation repaired or changed an unrestored page")
		}
	}
	for i, id := range ids {
		restore(id, manifest.Tabs[i])
		got := read(id)
		if math.Abs(got.ScrollX-manifest.Tabs[i].ScrollX) > 1 || math.Abs(got.ScrollY-manifest.Tabs[i].ScrollY) > 1 {
			t.Fatal("actual scroll restoration lost an axis")
		}
	}
	if err := verify(); err != nil {
		t.Fatal("restored real page set was not acknowledged", err)
	}
	t.Log("read-only rejection left zero scroll unchanged; both-axis restoration confirmed")

	// A same-origin document whose query or fragment differs is still not
	// the captured document. Restore its scroll first so a refusal cannot be
	// explained solely by navigation resetting the scroll coordinates.
	navigate := func(id, url string) {
		t.Helper()
		if err := withPage(ctx, id, func(c *cdpConn) error {
			out, err := c.Call("Page.navigate", map[string]any{"url": url})
			if err != nil {
				return err
			}
			if failed, _ := out["errorText"].(string); failed != "" {
				return errors.New("private fixture navigation failed")
			}
			return nil
		}); err != nil {
			t.Fatal("navigate private fixture page", err)
		}
		awaitSuspendFixture(t, ctx, "changed document discovery", func(ctx context.Context) bool {
			targets, err := list(ctx)
			if err != nil {
				return false
			}
			for _, target := range targets {
				if target.ID == id {
					return target.URL == url
				}
			}
			return false
		})
	}
	for _, changedURL := range []string{srv.URL + "/page?variant=changed#position", srv.URL + "/page?variant=one#changed"} {
		navigate(ids[0], changedURL)
		changed := manifest.Tabs[0]
		changed.URL = changedURL
		restore(ids[0], changed)
		if verify() == nil {
			t.Fatal("complete page observation ignored query or fragment drift")
		}
		got := read(ids[0])
		if got.URL != changedURL || math.Abs(got.ScrollX-changed.ScrollX) > 1 || math.Abs(got.ScrollY-changed.ScrollY) > 1 {
			t.Fatal("read-only observation changed the drifted page")
		}
		navigate(ids[0], manifest.Tabs[0].URL)
		restore(ids[0], manifest.Tabs[0])
		if err := verify(); err != nil {
			t.Fatal("reconciled document was not acknowledged", err)
		}
	}
	t.Log("query and fragment drift refused without repairing the page")

	extra := create(urls[0])
	awaitSuspendFixture(t, ctx, "extra page discovery", func(ctx context.Context) bool {
		targets, err := list(ctx)
		return err == nil && len(targets) == 4
	})
	if verify() == nil {
		t.Fatal("complete page observation silently omitted an extra page")
	}
	if browserCall("Target.closeTarget", map[string]any{"targetId": extra})["success"] != true {
		t.Fatal("close private extra fixture page failed")
	}
	awaitSuspendFixture(t, ctx, "original page population", func(ctx context.Context) bool {
		targets, err := list(ctx)
		return err == nil && len(targets) == 3
	})
	selected = ids[0]
	if verify() == nil {
		t.Fatal("restored page observations ignored a different selected target")
	}
	selected = ids[2]
	if err := verify(); err != nil {
		t.Fatal("closed extra page or selection reconciliation broke the original set", err)
	}
	// Close one of the duplicate-URL targets, not the selected page. The other
	// target with that URL must not stand in for the missing target identity.
	if browserCall("Target.closeTarget", map[string]any{"targetId": ids[1]})["success"] != true {
		t.Fatal("close private duplicate fixture page failed")
	}
	awaitSuspendFixture(t, ctx, "missing duplicate page discovery", func(ctx context.Context) bool {
		targets, err := list(ctx)
		return err == nil && len(targets) == 2
	})
	if verify() == nil {
		t.Fatal("complete page observation substituted a duplicate URL for a missing target")
	}
	t.Log("three real tabs, including duplicate URLs: capture, read-only refusal, both-axis restore, URL drift, complete-set and selection checks passed")
	// Exercise only graceful root exit, not suspend or all-resource teardown.
	// This local fixture fence permits the expected exit but still rejects a
	// changed proc/generation. StopAll cleanup remains responsible for the rest.
	closeFence := func() error {
		s.mu.Lock()
		current := s.proc == proc && s.generation == generation
		s.mu.Unlock()
		if !current || ctx.Err() != nil {
			return errors.New("private closing fixture incarnation changed")
		}
		return nil
	}
	select {
	case <-proc.exited:
		t.Fatal("private fixture root exited before the graceful request")
	default:
	}
	if err := closeSuspendBrowser(ctx, suspendBrowserClosePlan{port: proc.debugPort, browserID: proc.browserID, exited: proc.exited}, closeFence); err != nil {
		t.Fatal("private fixture graceful root exit failed", err)
	}
	select {
	case <-proc.exited:
	default:
		t.Fatal("graceful close acknowledged before the fixture root was reaped")
	}
	t.Log("bounded browser close observed the pinned root reaper; complete resource teardown is not claimed")
}

func awaitSuspendFixture(t *testing.T, parent context.Context, what string, ready func(context.Context) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if ready(ctx) {
			return
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	t.Fatal("private suspend fixture did not reach " + what)
}
