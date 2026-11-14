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
//
// Tracking is local mode only: a manager opts in with EnableTargetTracking. The
// node-wide manager does not, and keeps its single-page behavior unchanged.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

// MaxCobrowseTargets bounds the open pages of one session. A page that opens
// more is closed, newest first, so a popup flood cannot exhaust the browser.
const MaxCobrowseTargets = 8

const (
	maxCobrowseTargetURLBytes = 2048
	maxTargetStack            = 16
	originVerdictTTL          = 30 * time.Second
	targetSyncTimeout         = 15 * time.Second
)

var (
	// ErrTargetsDisabled is returned by page calls on a manager that does not
	// track pages (everything but local mode).
	ErrTargetsDisabled = errors.New("page tracking is not enabled for this session manager")
	// ErrTargetChanged refuses an action whose page stopped being the active
	// one between choosing it and sending input.
	ErrTargetChanged = errors.New("the active page changed while the action was being prepared; try again")
)

// CobrowseTarget is one open page of a session.
type CobrowseTarget struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	Active bool   `json:"active,omitempty"`
}

// CobrowseTargets is a snapshot of a session's open pages, in the order they
// were first seen (stable, not the browser's list order).
type CobrowseTargets struct {
	Active  string           `json:"active"`
	Targets []CobrowseTarget `json:"targets"`
	// ActiveWebSocketURL is the loopback debugger URL of the active page, for
	// the node-side stream only. It never leaves the node.
	ActiveWebSocketURL string `json:"-"`
}

// TargetPolicy decides whether a page may stay open. A non-nil error closes it.
type TargetPolicy = func(ctx context.Context, rawURL string) error

// TargetEventFunc is told about page events: "target_closed" (Citadel closed a
// page the policy or the bound refused), "target_refused" (a refused page was
// left open because it is the only one) and, for the user's own choices,
// "target_switch" and "target_close". It runs under the set's sync lock, so it
// must not call back into the manager.
type TargetEventFunc = func(sessionID, action, rawURL string, reason error)

type targetSet struct {
	// syncMu serializes whole synchronizations so two callers never close the
	// same page or reorder the opener stack.
	syncMu sync.Mutex

	mu        sync.Mutex
	sessionID string
	policy    TargetPolicy
	onEvent   TargetEventFunc
	active    string
	stack     []string          // openers to return to, most recent last
	order     []string          // ids present at the last sync, in first-seen order
	known     map[string]string // id -> last URL accepted (or reported) for a present page
	okOrigins map[string]time.Time
	list      []cdpdiscovery.Target // in order
}

func newTargetSet() *targetSet {
	return &targetSet{known: map[string]string{}, okOrigins: map[string]time.Time{}}
}

var targetSets sync.Map // debug port -> *targetSet

func targetSetFor(port int) *targetSet {
	if v, ok := targetSets.Load(port); ok {
		return v.(*targetSet)
	}
	v, _ := targetSets.LoadOrStore(port, newTargetSet())
	return v.(*targetSet)
}

func dropTargetSet(port int) { targetSets.Delete(port) }

// configure sets the page policy and event callback.
func (t *targetSet) configure(sessionID string, policy TargetPolicy, onEvent TargetEventFunc) {
	t.mu.Lock()
	t.sessionID, t.policy, t.onEvent = sessionID, policy, onEvent
	t.mu.Unlock()
}

// schemePolicy is the policy of a set nobody configured (a re-adopted session,
// a call racing teardown): only http(s) and about:blank pages may stay open, so
// file:, data: and browser-internal pages never become reachable.
func schemePolicy(_ context.Context, raw string) error { return ValidateCobrowseURL(raw) }

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// fetchTargetOpeners asks the browser which page opened which (Target.getTargets
// reports openerId; the HTTP /json list does not). It is a package variable so
// tests can stand in for a browser.
var fetchTargetOpeners = defaultFetchTargetOpeners

func defaultFetchTargetOpeners(ctx context.Context, port int) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return nil, err
	}
	client := *cdpHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("browser version probe failed")
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return nil, err
	}
	u, err := url.Parse(v.WS)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("browser endpoint invalid")
	}
	u.Scheme, u.Host = "ws", "127.0.0.1:"+strconv.Itoa(port)
	res, err := cdpDialAndSendContext(ctx, u.String(), "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	infos, _ := res["targetInfos"].([]any)
	out := make(map[string]string, len(infos))
	for _, raw := range infos {
		info, _ := raw.(map[string]any)
		id, _ := info["targetId"].(string)
		opener, _ := info["openerId"].(string)
		if id != "" {
			out[id] = opener
		}
	}
	return out, nil
}

// reconcileTargets is the pure tracking rule. prev and ids are the open pages
// before and now, in first-seen order. A page that disappears returns to the
// most recent opener still open (else the earliest page). A page that appears
// becomes active only when the browser reports the active page opened it; a
// page opened by a background tab, or whose opener is unknown, stays listed and
// is chosen explicitly. The first observation adopts the first page.
func reconcileTargets(active string, stack, prev, ids []string, openerOf func(id string) (string, bool)) (string, []string) {
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	kept := make([]string, 0, len(stack))
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
	if len(prev) > 0 {
		wasSeen := make(map[string]bool, len(prev))
		for _, id := range prev {
			wasSeen[id] = true
		}
		var fresh []string
		for _, id := range ids {
			if !wasSeen[id] && id != active {
				fresh = append(fresh, id)
			}
		}
		for changed := true; changed && len(fresh) > 0; {
			changed = false
			rest := fresh[:0]
			for _, id := range fresh {
				if op, ok := openerOf(id); ok && op != "" && op == active {
					stack = append(stack, active)
					active = id
					changed = true
					continue
				}
				rest = append(rest, id)
			}
			fresh = rest
		}
	}
	if len(stack) > maxTargetStack {
		stack = append([]string(nil), stack[len(stack)-maxTargetStack:]...)
	}
	return active, stack
}

// sync reads the browser's pages, closes any the policy refuses or the bound
// excludes, updates the active page and returns the snapshot.
func (t *targetSet) sync(ctx context.Context, port int) (CobrowseTargets, error) {
	t.syncMu.Lock()
	defer t.syncMu.Unlock()
	list, overflow, err := cdpdiscovery.ListBounded(ctx, port, cdpHTTPClient)
	if err != nil {
		return CobrowseTargets{}, err
	}
	t.mu.Lock()
	policy, onEvent, sessionID := t.policy, t.onEvent, t.sessionID
	prevOrder := append([]string(nil), t.order...)
	prevSeen := make(map[string]bool, len(prevOrder))
	for _, id := range prevOrder {
		prevSeen[id] = true
	}
	known := make(map[string]string, len(list))
	for id, u := range t.known {
		known[id] = u
	}
	okOrigins := t.okOrigins
	t.mu.Unlock()
	if policy == nil {
		policy = schemePolicy
	}

	// First-seen order, never the browser's list order.
	byID := make(map[string]cdpdiscovery.Target, len(list))
	for _, tg := range list {
		byID[tg.ID] = tg
	}
	ordered := make([]cdpdiscovery.Target, 0, len(list))
	for _, id := range prevOrder {
		if tg, ok := byID[id]; ok {
			ordered = append(ordered, tg)
			delete(byID, id)
		}
	}
	for _, tg := range list {
		if _, ok := byID[tg.ID]; ok {
			ordered = append(ordered, tg)
			delete(byID, tg.ID)
		}
	}

	type event struct {
		action, url string
		err         error
	}
	var closing []cdpdiscovery.Target
	var events []event
	kept := make([]cdpdiscovery.Target, 0, len(ordered))
	now := time.Now()
	for _, tg := range ordered {
		if known[tg.ID] == tg.URL && prevSeen[tg.ID] {
			kept = append(kept, tg)
			continue
		}
		// A page with no address yet (a fresh popup) is judged when it gets one.
		// Pages on an origin already judged recently are not re-resolved.
		origin := originOf(tg.URL)
		if tg.URL != "" && tg.URL != "about:blank" {
			if at, ok := okOrigins[origin]; !ok || origin == "" || now.Sub(at) > originVerdictTTL {
				pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				perr := policy(pctx, tg.URL)
				cancel()
				if perr != nil {
					closing = append(closing, tg)
					events = append(events, event{"target_closed", tg.URL, perr})
					continue
				}
				if origin != "" {
					okOrigins[origin] = now
				}
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
		events = append(events, event{"target_closed", kept[idx].URL, fmt.Errorf("too many open pages (limit %d)", MaxCobrowseTargets)})
		kept = append(kept[:idx], kept[idx+1:]...)
	}
	// Never close the last page: that would end the session. It is reported
	// once; the agent's own navigation checks cover a refused page that is all
	// there is.
	if len(kept) == 0 && len(closing) > 0 {
		kept = append(kept, closing[0])
		closing = closing[1:]
		for i := range events {
			if events[i].action == "target_closed" && events[i].url == kept[0].URL {
				events[i].action = "target_refused"
				break
			}
		}
		known[kept[0].ID] = kept[0].URL
	}
	for _, tg := range closing {
		_ = closeTargetByID(ctx, port, tg.ID)
	}

	ids := make([]string, len(kept))
	present := make(map[string]bool, len(kept))
	for i, tg := range kept {
		ids[i] = tg.ID
		present[tg.ID] = true
	}
	// Ask the browser who opened any new page, only when there is one.
	var openers map[string]string
	if len(prevOrder) > 0 {
		for _, id := range ids {
			if !prevSeen[id] {
				octx, cancel := context.WithTimeout(ctx, 10*time.Second)
				openers, _ = fetchTargetOpeners(octx, port)
				cancel()
				break
			}
		}
	}
	for id := range known {
		if !present[id] {
			delete(known, id)
		}
	}

	t.mu.Lock()
	t.active, t.stack = reconcileTargets(t.active, t.stack, prevOrder, ids, func(id string) (string, bool) {
		op, ok := openers[id]
		return op, ok
	})
	t.order, t.known, t.list = ids, known, kept
	for o, at := range t.okOrigins {
		if now.Sub(at) > originVerdictTTL {
			delete(t.okOrigins, o)
		}
	}
	snap := t.snapshotLocked()
	t.mu.Unlock()
	_ = overflow // pages past the cap are seen on later syncs as extras are closed
	if onEvent != nil {
		for _, e := range events {
			onEvent(sessionID, e.action, e.url, e.err)
		}
	}
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
		if len(t.stack) > maxTargetStack {
			t.stack = append([]string(nil), t.stack[len(t.stack)-maxTargetStack:]...)
		}
	}
	t.active = id
	return nil
}

func (t *targetSet) urlOf(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tg := range t.list {
		if tg.ID == id {
			return tg.URL
		}
	}
	return ""
}

func (t *targetSet) emit(action, rawURL string) {
	t.mu.Lock()
	f, id := t.onEvent, t.sessionID
	t.mu.Unlock()
	if f != nil {
		f(id, action, rawURL, nil)
	}
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

// EnableTargetTracking turns on page tracking for this manager's sessions
// launched afterwards: popups and new tabs are followed, every page must satisfy
// policy (nil means the http(s) scheme rule), and onEvent hears about pages
// Citadel closes and the user's own switches. A manager that never calls this
// keeps its original single-page behavior byte for byte, which is what the
// node-wide manager does.
func (m *CobrowseSessionManager) EnableTargetTracking(policy TargetPolicy, onEvent TargetEventFunc) {
	m.mu.Lock()
	m.trackTargets, m.targetPolicy, m.targetEvent = true, policy, onEvent
	m.mu.Unlock()
}

// TargetsEnabled reports whether this manager tracks pages.
func (m *CobrowseSessionManager) TargetsEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.trackTargets
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

func (m *CobrowseSessionManager) targetSession(id string) (*cobrowseSession, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return nil, err
	}
	if !s.tracking {
		return nil, ErrTargetsDisabled
	}
	return s, nil
}

// Targets reports the open pages of a session, following any popup or new tab.
func (m *CobrowseSessionManager) Targets(id string) (CobrowseTargets, error) {
	s, err := m.targetSession(id)
	if err != nil {
		return CobrowseTargets{}, err
	}
	s.mu.Lock()
	port, ok := s.targetPortLocked()
	s.mu.Unlock()
	if !ok {
		return CobrowseTargets{}, ErrNotStarted
	}
	ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
	defer cancel()
	return targetSetFor(port).sync(ctx, port)
}

func (m *CobrowseSessionManager) humanTargetAction(id string) (int, error) {
	s, err := m.targetSession(id)
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
// may choose a page; popups the active page opens are followed automatically.
func (m *CobrowseSessionManager) SwitchTarget(id, targetID string) error {
	port, err := m.humanTargetAction(id)
	if err != nil {
		return err
	}
	set := targetSetFor(port)
	ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
	defer cancel()
	if _, err := set.sync(ctx, port); err != nil {
		return err
	}
	if err := set.setActive(targetID); err != nil {
		return err
	}
	set.emit("target_switch", set.urlOf(targetID))
	return nil
}

// CloseTarget closes one page for the user who has taken control. The last
// page cannot be closed; stop the session instead.
func (m *CobrowseSessionManager) CloseTarget(id, targetID string) error {
	port, err := m.humanTargetAction(id)
	if err != nil {
		return err
	}
	set := targetSetFor(port)
	ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
	defer cancel()
	snap, err := set.sync(ctx, port)
	if err != nil {
		return err
	}
	if len(snap.Targets) <= 1 {
		return fmt.Errorf("this is the last page; stop the session to close it")
	}
	found := ""
	ok := false
	for _, tg := range snap.Targets {
		if tg.ID == targetID {
			found, ok = tg.URL, true
		}
	}
	if !ok {
		return fmt.Errorf("no such page %q in this session", targetID)
	}
	if err := closeTargetByID(ctx, port, targetID); err != nil {
		return err
	}
	set.emit("target_close", found)
	_, err = set.sync(ctx, port)
	return err
}

// ExpectTarget records the page an agent call was checked against (its URL
// policy, grant and approval). Until cleared with an empty targetID, every
// action on the session is refused with ErrTargetChanged if the active page is
// not that page, so the page that was checked is the page that gets the input.
func (m *CobrowseSessionManager) ExpectTarget(id, targetID string) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.expectTarget = targetID
	s.mu.Unlock()
}
