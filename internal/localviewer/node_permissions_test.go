package localviewer

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeNodePerms records every Set call.
type fakeNodePerms struct {
	mu    sync.Mutex
	state NodePermissionState
	sets  []string
}

func (f *fakeNodePerms) hooks() *NodePermissionHooks {
	return &NodePermissionHooks{
		Get: func() (NodePermissionState, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.state, nil
		},
		Set: func(surface string, enabled bool) (NodePermissionState, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.sets = append(f.sets, surface+"="+strconv.FormatBool(enabled))
			if surface == SurfaceFiles {
				f.state.Files = enabled
			} else {
				f.state.Shell = enabled
			}
			return f.state, nil
		},
	}
}

func (f *fakeNodePerms) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sets...)
}

// newPermsServer starts a viewer (or, with setup, a setup page) whose node
// permission hooks are fake.
func newPermsServer(t *testing.T, setup bool) (*Server, *fakeNodePerms, string) {
	t.Helper()
	fake := &fakeNodePerms{state: NodePermissionState{Available: true, PublicOnly: true, Workspace: "/home/demo/citadel-node/workspace"}}
	cfg := Config{TokenDir: t.TempDir(), NodePermissions: fake.hooks()}
	if setup {
		cfg.Setup = &SetupConfig{}
	}
	srv := New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, fake, "http://127.0.0.1:" + strconv.Itoa(srv.Port())
}

func permsRequest(t *testing.T, srv *Server, base, method, body string, hdr map[string]string, cookie bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, base+"/api/node-permissions", strings.NewReader(body))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if cookie {
		req.AddCookie(&http.Cookie{Name: srv.cookieName(), Value: srv.cookie})
	}
	resp, err := (&http.Client{CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestNodePermissionRoutesAreGuarded: on the viewer and on the setup page,
// the toggles need the exact Host and Origin, a same-origin fetch, the
// cookie and the bearer; no refused request changes anything.
func TestNodePermissionRoutesAreGuarded(t *testing.T) {
	for _, setup := range []bool{false, true} {
		srv, fake, base := newPermsServer(t, setup)
		good := map[string]string{"Origin": base, csrfHeader: srv.bearer, "Content-Type": "application/json"}
		with := func(k, v string) map[string]string {
			h := map[string]string{}
			for a, b := range good {
				h[a] = b
			}
			if v == "" {
				delete(h, k)
			} else {
				h[k] = v
			}
			return h
		}
		on := `{"confirm":true,"surface":"files","enabled":true}`
		cases := []struct {
			name, method, body string
			hdr                map[string]string
			cookie             bool
		}{
			{"get without bearer", "GET", "", with(csrfHeader, ""), true},
			{"get without cookie", "GET", "", good, false},
			{"get foreign origin", "GET", "", with("Origin", "http://attacker.test"), true},
			{"set without bearer", "POST", on, with(csrfHeader, ""), true},
			{"set wrong bearer", "POST", on, with(csrfHeader, "nope"), true},
			{"set without cookie", "POST", on, good, false},
			{"set foreign origin", "POST", on, with("Origin", "http://attacker.test"), true},
			{"set without origin", "POST", on, with("Origin", ""), true},
			{"set cross-site", "POST", on, with("Sec-Fetch-Site", "cross-site"), true},
			{"set same-site", "POST", on, with("Sec-Fetch-Site", "same-site"), true},
			{"set wrong host", "POST", on, with("Host", "localhost:"+strconv.Itoa(srv.Port())), true},
		}
		for _, c := range cases {
			resp := permsRequest(t, srv, base, c.method, c.body, c.hdr, c.cookie)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("setup=%v %s: want 403, got %d", setup, c.name, resp.StatusCode)
			}
		}
		if got := fake.calls(); len(got) != 0 {
			t.Fatalf("setup=%v: a refused request changed a permission: %v", setup, got)
		}
		// Positive control.
		resp := permsRequest(t, srv, base, "POST", on, good, true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("setup=%v: an authenticated, confirmed change: %d", setup, resp.StatusCode)
		}
		if got := fake.calls(); len(got) != 1 || got[0] != "files=true" {
			t.Fatalf("setup=%v: calls %v", setup, got)
		}
	}
}

// TestNodePermissionSetNeedsConfirmation: a change needs "confirm": true and
// an explicit value; allowing commands also needs "confirm_shell": true.
// Turning Shell off needs no second confirmation.
func TestNodePermissionSetNeedsConfirmation(t *testing.T) {
	srv, fake, base := newPermsServer(t, false)
	good := map[string]string{"Origin": base, csrfHeader: srv.bearer, "Content-Type": "application/json"}
	refused := []struct{ name, body string }{
		{"no confirm", `{"surface":"files","enabled":true}`},
		{"confirm false", `{"confirm":false,"surface":"files","enabled":true}`},
		{"no value", `{"confirm":true,"surface":"files"}`},
		{"unknown surface", `{"confirm":true,"surface":"console","enabled":true}`},
		{"shell without second confirmation", `{"confirm":true,"surface":"shell","enabled":true}`},
		{"shell with a false second confirmation", `{"confirm":true,"surface":"shell","enabled":true,"confirm_shell":false}`},
		{"not JSON", `confirm=true&surface=files&enabled=true`},
	}
	for _, c := range refused {
		hdr := good
		if c.name == "not JSON" {
			hdr = map[string]string{"Origin": base, csrfHeader: srv.bearer, "Content-Type": "application/x-www-form-urlencoded"}
		}
		resp := permsRequest(t, srv, base, "POST", c.body, hdr, true)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if got := fake.calls(); len(got) != 0 {
		t.Fatalf("a refused request changed a permission: %v", got)
	}
	if r := permsRequest(t, srv, base, "POST", `{"confirm":true,"surface":"shell","enabled":true,"confirm_shell":true}`, good, true); r.StatusCode != http.StatusOK {
		t.Fatalf("shell with both confirmations: %d", r.StatusCode)
	}
	if r := permsRequest(t, srv, base, "POST", `{"confirm":true,"surface":"shell","enabled":false}`, good, true); r.StatusCode != http.StatusOK {
		t.Fatalf("shell off: %d", r.StatusCode)
	}
	if got := fake.calls(); strings.Join(got, ",") != "shell=true,shell=false" {
		t.Fatalf("calls %v", got)
	}
	resp := permsRequest(t, srv, base, "GET", "", good, true)
	var st NodePermissionState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || !st.Available || st.Shell || !st.PublicOnly {
		t.Fatalf("state: %+v %v", st, err)
	}
}

// TestNodePermissionRoutesWithoutHooks: with no hooks the state reads as
// unavailable and a change is refused.
func TestNodePermissionRoutesWithoutHooks(t *testing.T) {
	srv := New(Config{TokenDir: t.TempDir()})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	base := "http://127.0.0.1:" + strconv.Itoa(srv.Port())
	good := map[string]string{"Origin": base, csrfHeader: srv.bearer, "Content-Type": "application/json"}
	if r := permsRequest(t, srv, base, "POST", `{"confirm":true,"surface":"files","enabled":true}`, good, true); r.StatusCode != http.StatusNotFound {
		t.Fatalf("change without hooks: %d", r.StatusCode)
	}
	resp := permsRequest(t, srv, base, "GET", "", good, true)
	var st NodePermissionState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Available {
		t.Fatalf("state without hooks: %+v %v", st, err)
	}
}
