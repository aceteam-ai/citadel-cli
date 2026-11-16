package platform

import (
	"context"
	"errors"
	"github.com/aceteam-ai/citadel-cli/internal/browseregress"
	"time"
)

// Guarded sessions cannot survive the in-process proxy's owner. A fresh session
// is required after restart; no browser is silently adopted without the guard.
const endedReasonEgressRestart = "egress_guard_restart_required"
const egressRestartDetail = "Start a new browser session to restore guarded network access."

var ErrEgressGuardUnavailable = errors.New("browser egress guard unavailable; start a new guarded session")

type sessionEgress interface {
	ProxyURL() string
	Active() bool
	Close()
	CheckURL(context.Context, string) error
	PrivateRefused() int64
}

func startSessionEgress() (sessionEgress, error) { return browseregress.StartEgressGuard(nil, nil) }

// EgressFeatures advertises enforcement for remote sessions created by this
// manager; it does not authorize private network access.
func (m *CobrowseSessionManager) EgressFeatures() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requireEgress {
		return []string{"egress_guard"}
	}
	return []string{}
}

func (s *cobrowseSession) checkEgressURL(raw string) error {
	s.mu.Lock()
	required, g := s.requireEgress, s.egress
	s.mu.Unlock()
	if !required {
		return nil
	}
	if g == nil || !g.Active() {
		return ErrEgressGuardUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return g.CheckURL(ctx, raw)
}
