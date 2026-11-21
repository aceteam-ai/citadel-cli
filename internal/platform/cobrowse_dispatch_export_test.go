package platform

import (
	"context"
	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
	"net/netip"
	"testing"
)

// DispatchFixture is test-only: no browser or OS process is launched.
type DispatchFixture struct {
	Manager  *CobrowseSessionManager
	Set      func(...[2]string)
	Calls    func(string) []string
	OnCall   func(func(string, string))
	SetTitle func(string, string)
}

func NewDispatchFixture(t *testing.T, tracking bool) *DispatchFixture {
	t.Helper()
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{"p": "a"})
	f.set([2]string{"a", "https://app.test/"})
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	if tracking {
		m.EnableTargetTracking(browserurl.NewURLPolicy(func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}, nil), nil)
	}
	t.Cleanup(func() { m.StopAll(); dropTargetSet(f.port) })
	return &DispatchFixture{
		Manager: m, Set: f.set, Calls: f.callsOn,
		OnCall: func(hook func(string, string)) { f.mu.Lock(); f.onCall = hook; f.mu.Unlock() },
		SetTitle: func(id, title string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, page := range f.pages {
				if page["id"] == id {
					page["title"] = title
				}
			}
		},
	}
}
