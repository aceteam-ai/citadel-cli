//go:build linux

package platform

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// realActionBudget is far above what these actions take on a hosted runner
// (well under a second) and far below the platform's 15s job wait, so a
// regression to per-call reconnects or unbounded waits fails here.
const realActionBudget = 5 * time.Second

// TestCobrowseRealBrowserActionLatency drives a real browser over a synthetic
// local page and asserts status, screenshot, click and type each finish inside
// realActionBudget and report their effects (citadel-cli#1663).
func TestCobrowseRealBrowserActionLatency(t *testing.T) {
	e := newRealEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>latency page</title><div style="height:3000px">tall</div><textarea id="t" rows="3"></textarea>`)
	}))
	t.Cleanup(srv.Close)
	m := e.manager()
	st := startReal(t, m, srv.URL)

	within := func(name string, f func() error) {
		t.Helper()
		start := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		el := time.Since(start)
		t.Logf("%s took %s", name, el)
		if el > realActionBudget {
			t.Errorf("%s took %s, budget %s", name, el, realActionBudget)
		}
	}

	within("status", func() error {
		got, ok := m.SessionStatus(st.ID)
		if !ok || got.URL == "" {
			return fmt.Errorf("no url in status: %+v", got)
		}
		return nil
	})
	within("screenshot", func() error {
		data, got, err := m.ScreenshotStatus(st.ID)
		if err == nil && (len(data) < 100 || got.URL == "") {
			err = fmt.Errorf("screenshot empty or no url")
		}
		return err
	})
	within("click", func() error {
		got, err := m.Click(st.ID, "#t", nil, nil)
		if err != nil {
			return err
		}
		eff := got.Effects
		if got.Done == nil || !*got.Done || eff == nil || !eff.Clicked || !eff.Scrolled || eff.ScrollY <= 0 || !eff.Focused || eff.FocusTag != "textarea" {
			return fmt.Errorf("click effects %+v done=%v", eff, got.Done)
		}
		return nil
	})
	within("type", func() error {
		got, err := m.Type(st.ID, "hello")
		if err != nil {
			return err
		}
		if got.Effects == nil || got.Effects.TypedLength != 5 || got.Effects.TypedUnconfirmed {
			return fmt.Errorf("type effects %+v", got.Effects)
		}
		return nil
	})
}
