package localcobrowse

import (
	"errors"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// pagedBrowser models a session with several pages. Like the real manager, it
// refuses any action when the page named by ExpectTarget is not the active one,
// and it records which page received input.
type pagedBrowser struct {
	*fakeBrowser
	pmu    sync.Mutex
	pages  map[string]string // page id -> URL
	order  []string
	active string
	expect string
	input  []string // page ids that received a click or key
	// onProbe runs (outside the lock) at the start of every InspectTarget.
	onProbe func(n int)
	probes  int
}

func newPagedBrowser(fb *fakeBrowser) *pagedBrowser {
	return &pagedBrowser{fakeBrowser: fb, pages: map[string]string{"a": "http://a.test/form"}, order: []string{"a"}, active: "a"}
}

func (p *pagedBrowser) open(id, url string, activate bool) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.pages[id] = url
	p.order = append(p.order, id)
	if activate {
		p.active = id
	}
}

func (p *pagedBrowser) closePage(id string) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	delete(p.pages, id)
	for i, o := range p.order {
		if o == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if p.active == id && len(p.order) > 0 {
		p.active = p.order[0]
	}
}

func (p *pagedBrowser) check() error {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.expect != "" && p.expect != p.active {
		return platform.ErrTargetChanged
	}
	return nil
}

func (p *pagedBrowser) ExpectTarget(id, target string) {
	p.pmu.Lock()
	p.expect = target
	p.pmu.Unlock()
}

func (p *pagedBrowser) SessionStatus(id string) (platform.CobrowseSessionStatus, bool) {
	st, ok := p.fakeBrowser.SessionStatus(id)
	if !ok {
		return st, ok
	}
	p.pmu.Lock()
	defer p.pmu.Unlock()
	st.URL, st.ActiveTarget = p.pages[p.active], p.active
	st.Targets = nil
	for _, o := range p.order {
		st.Targets = append(st.Targets, platform.CobrowseTarget{ID: o, URL: p.pages[o], Active: o == p.active})
	}
	return st, true
}

func (p *pagedBrowser) PageState(id string) (platform.PageState, error) {
	if err := p.check(); err != nil {
		return platform.PageState{}, err
	}
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return platform.PageState{URL: p.pages[p.active], ReadyState: "complete"}, nil
}

func (p *pagedBrowser) InspectTarget(id string, pr platform.TargetProbe) (platform.TargetInfo, error) {
	p.pmu.Lock()
	p.probes++
	n := p.probes
	hook := p.onProbe
	p.pmu.Unlock()
	if hook != nil {
		hook(n)
	}
	if err := p.check(); err != nil {
		return platform.TargetInfo{}, err
	}
	t, err := p.fakeBrowser.InspectTarget(id, pr)
	p.pmu.Lock()
	t.PageURL = p.pages[p.active]
	p.pmu.Unlock()
	return t, err
}

func (p *pagedBrowser) Click(id, sel string, x, y *float64) (platform.CobrowseSessionStatus, error) {
	if err := p.check(); err != nil {
		return platform.CobrowseSessionStatus{}, err
	}
	p.pmu.Lock()
	p.input = append(p.input, p.active)
	p.pmu.Unlock()
	return p.fakeBrowser.Click(id, sel, x, y)
}

func (p *pagedBrowser) Key(id, key string) (platform.CobrowseSessionStatus, error) {
	if err := p.check(); err != nil {
		return platform.CobrowseSessionStatus{}, err
	}
	p.pmu.Lock()
	p.input = append(p.input, p.active)
	p.pmu.Unlock()
	return p.fakeBrowser.Key(id, key)
}

func (p *pagedBrowser) inputs() []string {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return append([]string(nil), p.input...)
}

func startPaged(t *testing.T) (*Service, *pagedBrowser, *fakeBrowser, string) {
	t.Helper()
	var pb *pagedBrowser
	svc, fb, _, id := startGranted(t, func(c *Config) {
		pb = newPagedBrowser(c.Browser.(*fakeBrowser))
		c.Browser = pb
	})
	return svc, pb, fb, id
}

// A popup that opens after the grant check and before the click never receives
// the click: the checked page is the only page the input can go to.
func TestPopupOpenedBetweenGrantAndClickReceivesNoInput(t *testing.T) {
	svc, pb, _, id := startPaged(t)
	pb.onProbe = func(n int) {
		if n == 2 { // after classification, at the re-probe just before dispatch
			pb.open("p", "http://a.test/popup", true)
		}
	}
	if _, err := svc.Click(ctx, id, "#go", nil, nil, "g"); !errors.Is(err, platform.ErrTargetChanged) {
		t.Fatalf("click after a popup opened: %v", err)
	}
	if got := pb.inputs(); len(got) != 0 {
		t.Fatalf("input reached %v", got)
	}
	// The next call re-resolves and addresses the popup, with its own grant check.
	if _, err := svc.Click(ctx, id, "#go", nil, nil, "g"); err != nil {
		t.Fatalf("next action: %v", err)
	}
	if got := pb.inputs(); len(got) != 1 || got[0] != "p" {
		t.Fatalf("next action went to %v", got)
	}
}

// A grant on domain A does not authorize input that would land on a popup on B.
func TestGrantOnOneDomainDoesNotCoverAPopupOnAnother(t *testing.T) {
	svc, pb, _, id := startPaged(t)
	pb.open("p", "http://b.test/login", true)
	_, err := svc.Click(ctx, id, "#go", nil, nil, "g")
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("click on a popup of another domain: %v", err)
	}
	if got := pb.inputs(); len(got) != 0 {
		t.Fatalf("input reached %v", got)
	}
	// The page the popup replaced is still covered when it is active again.
	pb.closePage("p")
	if _, err := svc.Click(ctx, id, "#go", nil, nil, "g"); err != nil {
		t.Fatalf("click on the granted page: %v", err)
	}
	if got := pb.inputs(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("input went to %v", got)
	}
}

// A grant check made on A cannot be spent on B if the popup opens mid-call.
func TestDomainSwitchMidCallIsRefusedNotRegranted(t *testing.T) {
	svc, pb, _, id := startPaged(t)
	pb.onProbe = func(n int) {
		if n == 1 {
			pb.open("p", "http://b.test/", true)
		}
	}
	if _, err := svc.Click(ctx, id, "#go", nil, nil, "g"); !errors.Is(err, platform.ErrTargetChanged) {
		t.Fatalf("err = %v", err)
	}
	if got := pb.inputs(); len(got) != 0 {
		t.Fatalf("input reached %v", got)
	}
}

// An approved commit runs only on the page it was recorded on.
func TestApprovedCommitIsNotExecutedOnAnotherPage(t *testing.T) {
	for name, move := range map[string]func(*pagedBrowser){
		"popup active":  func(pb *pagedBrowser) { pb.open("p", "http://a.test/popup", true) },
		"page gone":     func(pb *pagedBrowser) { pb.open("p", "http://a.test/popup", false); pb.closePage("a") },
		"another popup": func(pb *pagedBrowser) { pb.open("p", "http://b.test/", true) },
	} {
		t.Run(name, func(t *testing.T) {
			svc, pb, fb, id := startPaged(t)
			fb.target = sendTarget()
			if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
				t.Fatalf("send must ask: %v", err)
			}
			c := pendingView(t, svc, id)
			move(pb)
			if err := svc.ApproveCommit(ctx, id, c.ID); err != nil {
				t.Fatal(err)
			}
			if got := pb.inputs(); len(got) != 0 {
				t.Fatalf("approved commit ran on %v", got)
			}
			v := svc.Sessions(id)[0]
			if v.CommitResult == nil || v.CommitResult.State != "changed" {
				t.Fatalf("commit must resolve as changed: %+v", v.CommitResult)
			}
		})
	}

	// On the recorded page it still executes, once.
	svc, pb, fb, id := startPaged(t)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	c := pendingView(t, svc, id)
	if err := svc.ApproveCommit(ctx, id, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := pb.inputs(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("approved commit went to %v", got)
	}
}
