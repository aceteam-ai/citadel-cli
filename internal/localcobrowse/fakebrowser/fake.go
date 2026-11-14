// Package fakebrowser is an in-memory localcobrowse.Browser for tests in other
// packages. It honors the human driver the way the real session manager does.
package fakebrowser

import (
	"errors"
	"fmt"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

var _ localcobrowse.Browser = (*Fake)(nil)

// Fake is a test Browser.
type Fake struct {
	mu       sync.Mutex
	n        int
	sessions map[string]*sess
	// Calls records browser calls in order.
	Calls []string
	// Target, when set, is what InspectTarget reports for every action. The
	// default is a plain, resolved, non-interactive element on the current page.
	Target *platform.TargetInfo
}

type sess struct {
	url   string
	human bool
}

// New returns an empty Fake.
func New() *Fake { return &Fake{sessions: map[string]*sess{}} }

func (f *Fake) st(id string) platform.CobrowseSessionStatus {
	s := f.sessions[id]
	d := platform.DriverAI
	if s.human {
		d = platform.DriverHuman
	}
	return platform.CobrowseSessionStatus{ID: id, State: platform.SessionRunning, Driver: d, URL: s.url, Browser: "edge", DebugPort: 1}
}

func (f *Fake) agent(id, call string) (*sess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such browser session: %q", id)
	}
	if s.human {
		return nil, platform.ErrHandedOff
	}
	f.Calls = append(f.Calls, call)
	return s, nil
}

func (f *Fake) StartSession(u string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("cb-%d", f.n)
	f.sessions[id] = &sess{url: u}
	f.Calls = append(f.Calls, "start "+u)
	return f.st(id), nil
}
func (f *Fake) SessionStatus(id string) (platform.CobrowseSessionStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return platform.CobrowseSessionStatus{}, false
	}
	return f.st(id), true
}
func (f *Fake) Stop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	return nil
}
func (f *Fake) StopAll() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]*sess{}
	return nil
}
func (f *Fake) status(id string, err error) (platform.CobrowseSessionStatus, error) {
	if err != nil {
		return platform.CobrowseSessionStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st(id), nil
}
func (f *Fake) Navigate(id, u string) (platform.CobrowseSessionStatus, error) {
	s, err := f.agent(id, "navigate "+u)
	if err == nil {
		f.mu.Lock()
		s.url = u
		f.mu.Unlock()
	}
	return f.status(id, err)
}
func (f *Fake) NavigateAsHuman(id, u string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	s, ok := f.sessions[id]
	if !ok || !s.human {
		f.mu.Unlock()
		return platform.CobrowseSessionStatus{}, errors.New("the user has not taken control")
	}
	s.url = u
	f.Calls = append(f.Calls, "human-navigate "+u)
	f.mu.Unlock()
	return f.status(id, nil)
}
func (f *Fake) Click(id, sel string, _, _ *float64) (platform.CobrowseSessionStatus, error) {
	_, err := f.agent(id, "click "+sel)
	return f.status(id, err)
}
func (f *Fake) Type(id, _ string) (platform.CobrowseSessionStatus, error) {
	_, err := f.agent(id, "type")
	return f.status(id, err)
}
func (f *Fake) Select(id, sel, v, l string) (platform.SelectResult, error) {
	_, err := f.agent(id, "select "+sel)
	return platform.SelectResult{Value: v, Label: l}, err
}
func (f *Fake) Key(id, k string) (platform.CobrowseSessionStatus, error) {
	_, err := f.agent(id, "key "+k)
	return f.status(id, err)
}
func (f *Fake) Scroll(id, _ string, _, _ *float64, _, _ float64) (platform.CobrowseSessionStatus, error) {
	_, err := f.agent(id, "scroll")
	return f.status(id, err)
}
func (f *Fake) ExtractBounded(id, sel string, _ []string, _ int) (platform.ExtractResult, error) {
	_, err := f.agent(id, "extract "+sel)
	return platform.ExtractResult{Text: "fixture"}, err
}
func (f *Fake) Screenshot(id string) (string, error) {
	_, err := f.agent(id, "screenshot")
	return "iVBORw0KGgo=", err
}
func (f *Fake) PageState(id string) (platform.PageState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return platform.PageState{}, errors.New("no session")
	}
	if s.human {
		return platform.PageState{}, platform.ErrHandedOff
	}
	return platform.PageState{URL: s.url, ReadyState: "complete"}, nil
}
func (f *Fake) Handoff(id string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	if s, ok := f.sessions[id]; ok {
		s.human = true
	}
	f.mu.Unlock()
	return f.status(id, nil)
}
func (f *Fake) Resume(id string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	if s, ok := f.sessions[id]; ok {
		s.human = false
	}
	f.mu.Unlock()
	return f.status(id, nil)
}

// InspectTarget reports Target, or a plain element on the current page.
func (f *Fake) InspectTarget(id string, _ platform.TargetProbe) (platform.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return platform.TargetInfo{}, errors.New("no session")
	}
	if s.human {
		return platform.TargetInfo{}, platform.ErrHandedOff
	}
	if f.Target != nil {
		t := *f.Target
		if t.PageURL == "" {
			t.PageURL = s.url
		}
		return t, nil
	}
	return platform.TargetInfo{Resolved: true, Tag: "div", PageURL: s.url}, nil
}
