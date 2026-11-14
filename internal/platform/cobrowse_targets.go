// Page targets (tabs and popups) of a co-browse session.
//
// A page can open another page with window.open or a target=_blank link, for
// example a third-party sign-in popup that posts a token back to its opener and
// closes itself. Every scripted action and the viewer stream must follow the
// page the user is looking at, so each session keeps one targetSet: the page
// targets the browser reports, which one is active, and the pages to return to
// when the active one closes. The set is shared by agent actions and by every
// viewer (local and, later, the cloud one) through the manager's Targets,
// SwitchTarget and CloseTarget methods.
package platform

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

// MaxCobrowseTargets bounds the open pages of one session. A page that opens
// more is closed, newest first, so a popup flood cannot exhaust the browser.
const MaxCobrowseTargets = 8

const maxCobrowseTargetURLBytes = 2048

// CobrowseTarget is one open page of a session.
type CobrowseTarget struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	Active bool   `json:"active,omitempty"`
}

// CobrowseTargets is a snapshot of a session's open pages.
type CobrowseTargets struct {
	Active  string           `json:"active"`
	Targets []CobrowseTarget `json:"targets"`
	// ActiveWebSocketURL is the loopback debugger URL of the active page, for
	// the node-side stream only. It never leaves the node.
	ActiveWebSocketURL string `json:"-"`
}

// TargetPolicy decides whether a page may stay open. A non-nil error closes it.
type TargetPolicy = func(ctx context.Context, rawURL string) error

// TargetRefusedFunc is told when a page was closed by the policy or the bound.
type TargetRefusedFunc = func(sessionID, rawURL string, reason error)

type targetSet struct {
	// syncMu serializes whole synchronizations so two callers never close the
	// same page or reorder the opener stack.
	syncMu sync.Mutex

	mu        sync.Mutex
	sessionID string
	policy    TargetPolicy
	onRefused TargetRefusedFunc
	active    string
	stack     []string          // openers to return to, most recent last
	seen      map[string]bool   // ids present at the last synchronization
	known     map[string]string // id -> last URL the policy accepted
	list      []cdpdiscovery.Target
}

var targetSets sync.Map // debug port -> *targetSet

func targetSetFor(port int) *targetSet {
	if v, ok := targetSets.Load(port); ok {
		return v.(*targetSet)
	}
	v, _ := targetSets.LoadOrStore(port, &targetSet{seen: map[string]bool{}, known: map[string]string{}})
	return v.(*targetSet)
}

func lookupTargetSet(port int) *targetSet {
	if v, ok := targetSets.Load(port); ok {
		return v.(*targetSet)
	}
	return nil
}

func dropTargetSet(port int) { targetSets.Delete(port) }

func (t *targetSet) configure(sessionID string, policy TargetPolicy, onRefused TargetRefusedFunc) {
	t.mu.Lock()
	t.sessionID, t.policy, t.onRefused = sessionID, policy, onRefused
	t.mu.Unlock()
}

// reconcileTargets is the pure tracking rule. ids are the pages the browser
// reports now, in its order. A page that disappears returns to the most recent
// opener still open. A page that appears while another is active is treated as
// opened by it: it becomes active and the old page is pushed as its opener. The
// first observation just adopts the first page.
func reconcileTargets(active string, stack []string, seen map[string]bool, ids []string) (string, []string, map[string]bool) {
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	kept := stack[:0:0]
	for _, id := range stack {
		if present[id] && id != active {
			kept = append(kept, id)
		}
	}
	stack = kept
	if !present[active] {
		active = ""
		for len(stack) > 0 {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if present[last] {
				active = last
				break
			}
		}
		if active == "" && len(ids) > 0 {
			active = ids[0]
		}
	}
	if len(seen) > 0 {
		for _, id := range ids {
			if seen[id] || id == active {
				continue
			}
			if active != "" {
				stack = append(stack, active)
			}
			active = id
		}
	}
	return active, stack, present
}

// sync reads the browser's pages, closes any the policy refuses or the bound
// excludes, updates the active page and returns the snapshot.
func (t *targetSet) sync(ctx context.Context, port int) (CobrowseTargets, error) {
	t.syncMu.Lock()
	defer t.syncMu.Unlock()
	list, err := cdpdiscovery.List(ctx, port, cdpHTTPClient)
	if err != nil {
		return CobrowseTargets{}, err
	}
	t.mu.Lock()
	policy, onRefused, sessionID := t.policy, t.onRefused, t.sessionID
	prevSeen := t.seen
	known := make(map[string]string, len(t.known))
	for k, v := range t.known {
		known[k] = v
	}
	t.mu.Unlock()

	type refusal struct {
		url string
		err error
	}
	var closing []cdpdiscovery.Target
	var refusals []refusal
	kept := make([]cdpdiscovery.Target, 0, len(list))
	for _, tg := range list {
		if known[tg.ID] == tg.URL && prevSeen[tg.ID] {
			kept = append(kept, tg)
			continue
		}
		if policy != nil {
			pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			perr := policy(pctx, tg.URL)
			cancel()
			if perr != nil {
				closing = append(closing, tg)
				refusals = append(refusals, refusal{tg.URL, perr})
				continue
			}
		}
		known[tg.ID] = tg.URL
		kept = append(kept, tg)
	}
	// Bound the open pages: keep the oldest, close the newest new pages.
	for len(kept) > MaxCobrowseTargets {
		idx := -1
		for i := len(kept) - 1; i >= 0; i-- {
			if !prevSeen[kept[i].ID] {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		closing = append(closing, kept[idx])
		refusals = append(refusals, refusal{kept[idx].URL, fmt.Errorf("too many open pages (limit %d)", MaxCobrowseTargets)})
		kept = append(kept[:idx], kept[idx+1:]...)
	}
	// Never close the last page: that would end the session. The agent's own
	// navigation checks cover a refused page that is all there is.
	if len(kept) == 0 && len(closing) > 0 {
		kept = append(kept, closing[0])
		closing, refusals = closing[1:], refusals[1:]
		delete(known, kept[0].ID)
	}
	for _, tg := range closing {
		_ = closeTargetByID(ctx, port, tg.ID)
		delete(known, tg.ID)
	}
	if onRefused != nil {
		for _, r := range refusals {
			onRefused(sessionID, r.url, r.err)
		}
	}

	ids := make([]string, len(kept))
	for i, tg := range kept {
		ids[i] = tg.ID
	}
	t.mu.Lock()
	t.active, t.stack, t.seen = reconcileTargets(t.active, t.stack, t.seen, ids)
	t.known = known
	t.list = kept
	snap := t.snapshotLocked()
	t.mu.Unlock()
	return snap, nil
}

func (t *targetSet) snapshotLocked() CobrowseTargets {
	out := CobrowseTargets{Active: t.active, Targets: make([]CobrowseTarget, 0, len(t.list))}
	for _, tg := range t.list {
		u := tg.URL
		if len(u) > maxCobrowseTargetURLBytes {
			u = u[:maxCobrowseTargetURLBytes]
		}
		out.Targets = append(out.Targets, CobrowseTarget{ID: tg.ID, Title: SanitizeCobrowseTitle(tg.Title), URL: u, Active: tg.ID == t.active})
		if tg.ID == t.active {
			out.ActiveWebSocketURL = tg.WebSocketDebuggerURL
		}
	}
	return out
}

// activeTarget synchronizes and returns the page scripted actions should use.
func (t *targetSet) activeTarget(ctx context.Context, port int) (cdpdiscovery.Target, error) {
	snap, err := t.sync(ctx, port)
	if err != nil {
		return cdpdiscovery.Target{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tg := range t.list {
		if tg.ID == snap.Active {
			return tg, nil
		}
	}
	return cdpdiscovery.Target{}, fmt.Errorf("no page target found on CDP port %d", port)
}

// setActive makes id the active page, remembering the previous one as its
// opener so closing id returns there.
func (t *targetSet) setActive(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	found := false
	for _, tg := range t.list {
		if tg.ID == id {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no such page %q in this session", id)
	}
	if t.active == id {
		return nil
	}
	if t.active != "" {
		t.stack = append(t.stack, t.active)
	}
	t.active = id
	return nil
}

func validTargetID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// closeTargetByID closes a page through the browser's local DevTools endpoint.
func closeTargetByID(ctx context.Context, port int, id string) error {
	if !validTargetID(id) {
		return fmt.Errorf("invalid page id")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", port, id), nil)
	if err != nil {
		return err
	}
	client := *cdpHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("browser refused to close the page")
	}
	return nil
}

// SetTargetPolicy sets the policy every page a session opens must satisfy, and
// a callback for pages it closes. Applies to sessions launched afterwards.
func (m *CobrowseSessionManager) SetTargetPolicy(policy TargetPolicy, onRefused TargetRefusedFunc) {
	m.mu.Lock()
	m.targetPolicy, m.targetRefused = policy, onRefused
	m.mu.Unlock()
}

func (s *cobrowseSession) targetPortLocked() (int, bool) {
	if s.proc == nil {
		return 0, false
	}
	select {
	case <-s.proc.exited:
		return 0, false
	default:
	}
	return s.proc.debugPort, true
}

// Targets reports the open pages of a session, following any popup or new tab.
func (m *CobrowseSessionManager) Targets(id string) (CobrowseTargets, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseTargets{}, err
	}
	s.mu.Lock()
	port, ok := s.targetPortLocked()
	s.mu.Unlock()
	if !ok {
		return CobrowseTargets{}, ErrNotStarted
	}
	return targetSetFor(port).sync(context.Background(), port)
}

func (m *CobrowseSessionManager) humanTargetAction(id string) (int, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	port, ok := s.targetPortLocked()
	if !ok {
		return 0, ErrNotStarted
	}
	if !s.humanDrivingLocked() {
		return 0, fmt.Errorf("the user has not taken control of this session")
	}
	return port, nil
}

// SwitchTarget makes a page the active one. Only the user who has taken control
// may choose a page; popups the agent triggers are followed automatically.
func (m *CobrowseSessionManager) SwitchTarget(id, targetID string) error {
	port, err := m.humanTargetAction(id)
	if err != nil {
		return err
	}
	set := targetSetFor(port)
	if _, err := set.sync(context.Background(), port); err != nil {
		return err
	}
	return set.setActive(targetID)
}

// CloseTarget closes one page for the user who has taken control. The last
// page cannot be closed; stop the session instead.
func (m *CobrowseSessionManager) CloseTarget(id, targetID string) error {
	port, err := m.humanTargetAction(id)
	if err != nil {
		return err
	}
	set := targetSetFor(port)
	snap, err := set.sync(context.Background(), port)
	if err != nil {
		return err
	}
	if len(snap.Targets) <= 1 {
		return fmt.Errorf("this is the last page; stop the session to close it")
	}
	found := false
	for _, tg := range snap.Targets {
		if tg.ID == targetID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no such page %q in this session", targetID)
	}
	if err := closeTargetByID(context.Background(), port, targetID); err != nil {
		return err
	}
	_, err = set.sync(context.Background(), port)
	return err
}
