package platform

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

// SessionTargetsEnabled is per-session: an adopted legacy browser keeps its mode.
func (m *CobrowseSessionManager) SessionTargetsEnabled(id string) bool {
	s, err := m.sessionForAction(id)
	return err == nil && s.tracking
}

// WithTargetAssertion serializes dispatched actions and scopes their assertion.
// It never selects a page. Viewers may still switch pages, which the pin checks.
func (m *CobrowseSessionManager) WithTargetAssertion(id, target string, action func() error) error {
	s, err := m.sessionForAction(id)
	if err != nil {
		return err
	}
	s.actionMu.Lock()
	defer s.actionMu.Unlock()
	if target != "" && !s.tracking {
		return ErrTargetsDisabled
	}
	if target != "" {
		s.mu.Lock()
		port, ok := s.targetPortLocked()
		s.mu.Unlock()
		if !ok {
			return ErrNotStarted
		}
		set := targetSetFor(port)
		ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
		snap, err := set.sync(ctx, port)
		cancel()
		if err != nil {
			return err
		}
		set.mu.Lock()
		refused := set.refusedID
		set.mu.Unlock()
		if snap.Active != target && !(snap.Active == "" && refused == target) {
			return ErrTargetChanged
		}
	}
	s.mu.Lock()
	previous := s.expectTarget
	s.expectTarget = target
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.expectTarget = previous; s.mu.Unlock() }()
	return action()
}

// RecordTargetOpenedBy is display provenance, never authorization. Creation
// callers may annotate an existing approved page; unannotated pages
// are start (initial page) or page (browser-created popup).
func (m *CobrowseSessionManager) RecordTargetOpenedBy(id, target, by string) error {
	if by != "agent" && by != "user" {
		return fmt.Errorf("invalid page opener")
	}
	s, err := m.targetSession(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	port, ok := s.targetPortLocked()
	s.mu.Unlock()
	if !ok {
		return ErrNotStarted
	}
	t := targetSetFor(port)
	ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
	defer cancel()
	if _, err := t.readTarget(ctx, port, target); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, page := range t.list {
		if page.ID == target && target != t.refusedID {
			t.openedBy[target] = by
			return nil
		}
	}
	return ErrTargetChanged
}

func (s *cobrowseSession) pinRead(id string) (pin, CobrowseTarget, error) {
	if id == "" {
		p, err := s.pinAction()
		if err != nil {
			return p, CobrowseTarget{}, err
		}
		info := CobrowseTarget{ID: p.id}
		if p.set != nil {
			p.set.mu.Lock()
			for _, tg := range p.set.list {
				if tg.ID == p.id {
					info.URL = tg.URL
					info.Title = SanitizeCobrowseTitle(tg.Title)
				}
			}
			p.set.mu.Unlock()
		}
		return p, info, nil
	}
	port, err := s.requireDrivablePort()
	if err != nil {
		return pin{}, CobrowseTarget{}, err
	}
	if !s.tracking {
		return pin{}, CobrowseTarget{}, ErrTargetsDisabled
	}
	set := targetSetFor(port)
	ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
	defer cancel()
	tg, err := set.readTarget(ctx, port, id)
	if err != nil {
		return pin{}, CobrowseTarget{}, err
	}
	p := pin{port: port, id: tg.ID, ws: tg.WebSocketDebuggerURL, set: set, session: s, background: true}
	return p, CobrowseTarget{ID: tg.ID, URL: tg.URL, Title: SanitizeCobrowseTitle(tg.Title)}, nil
}

func (t *targetSet) readTarget(ctx context.Context, port int, id string) (cdpdiscovery.Target, error) {
	if _, err := t.sync(ctx, port); err != nil {
		return cdpdiscovery.Target{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == t.refusedID {
		return cdpdiscovery.Target{}, ErrPageRefused
	}
	for _, tg := range t.list {
		if tg.ID == id {
			return tg, nil
		}
	}
	return cdpdiscovery.Target{}, fmt.Errorf("no such page %q in this session", id)
}

// check fences the final awaited preparation before a read or first input.
func (p pin) check() error {
	if !p.refused {
		if err := p.verify(); err != nil {
			return err
		}
	}
	if p.session != nil {
		port, err := p.session.requireDrivablePort()
		if err != nil {
			return err
		}
		if port != p.port {
			return ErrNotStarted
		}
	}
	return nil
}

// screenshotPin retains the remote read path for an explicitly pinned page.
func screenshotPin(p pin) (string, error) {
	res, err := p.cdp("Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	data, _ := res["data"].(string)
	if data == "" {
		return "", fmt.Errorf("empty screenshot data from CDP")
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "", fmt.Errorf("screenshot not valid base64: %w", err)
	}
	return data, nil
}

// ScreenshotTarget reads an approved page without switching the active page.
func (m *CobrowseSessionManager) ScreenshotTarget(id, target string) (string, CobrowseTarget, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return "", CobrowseTarget{}, err
	}
	p, info, err := s.pinRead(target)
	if err != nil {
		return "", info, err
	}
	if err = p.check(); err != nil {
		return "", info, err
	}
	data, err := screenshotPin(p)
	if err != nil {
		return "", info, err
	}
	if err = p.check(); err != nil {
		return "", info, err
	}
	return data, info, nil
}

// ExtractTarget reads an approved background page; it never changes active.
func (m *CobrowseSessionManager) ExtractTarget(id, target, selector string, attrs []string) (ExtractResult, CobrowseTarget, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return ExtractResult{}, CobrowseTarget{}, err
	}
	p, info, err := s.pinRead(target)
	if err != nil {
		return ExtractResult{}, info, err
	}
	if err = p.check(); err != nil {
		return ExtractResult{}, info, err
	}
	out, err := extractElement(p, selector, attrs)
	if err != nil {
		return ExtractResult{}, info, err
	}
	if err = p.check(); err != nil {
		return ExtractResult{}, info, err
	}
	return out, info, nil
}
