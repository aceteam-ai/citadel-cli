package declaredsession

import (
	"encoding/json"
	"path/filepath"
)

type LogKind string

const (
	ConsoleLog LogKind = "console.log"
	EventsLog  LogKind = "events.jsonl"
)

type Logs struct {
	session *SessionStore
	policy  LogPolicy
}

// OpenLogs validates policy only; it creates no files until an explicit append.
// Console bytes are sensitive local history, never an event/status projection.
func (s *SessionStore) OpenLogs(policy LogPolicy) (*Logs, error) {
	if err := validateLogPolicy(policy); err != nil {
		return nil, err
	}
	return &Logs{session: s, policy: policy}, nil
}

func (l *Logs) AppendConsole(raw []byte) error {
	return l.session.withRecord(func() error {
		return appendLogAt(l.session.dir, string(ConsoleLog), raw, l.policy, l.session.store.uid)
	})
}

func (l *Logs) AppendEvent(event Event) error {
	if !oneOf(event.Kind, "spawn", "exit", "restart_scheduled", "attach", "detach", "controller_changed", "reload", "drift", "lease_result", "stop", "storage_failure") || event.At.IsZero() || !validIncarnation(event.Incarnation) || len(event.ViewerID) > 128 || (event.Role != "" && !oneOf(event.Role, "interactive", "readonly", "controller", "viewer")) || !oneOf(event.Reason, "", "protocol_error", "slow_viewer", "viewer_detached", "controller_transfer", "child_exit", "child_failure", "operator_stop", "operator_restart", "config_changed", "storage_failure", "cleanup_unconfirmed") {
		return ErrUnsafe
	}
	for _, ch := range event.ViewerID {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' && ch != '.' {
			return ErrUnsafe
		}
	}
	if (event.ResolvedExecutable != "" && !filepath.IsAbs(event.ResolvedExecutable)) || (event.EnvironmentHash != "" && !validDigest(event.EnvironmentHash)) {
		return ErrUnsafe
	}
	raw, err := json.Marshal(event)
	if err != nil || len(raw)+1 > MaxEventBytes {
		return ErrUnsafe
	}
	raw = append(raw, '\n')
	return l.session.withRecord(func() error {
		return appendLogAt(l.session.dir, string(EventsLog), raw, l.policy, l.session.store.uid)
	})
}

// Tail reads only the named current log, not arbitrary paths or support bundles.
// Rotated archives stay private and bounded by LogPolicy. No files are created.
func (s *SessionStore) ReadLogTail(kind LogKind, maxBytes int) ([]byte, error) {
	if (kind != ConsoleLog && kind != EventsLog) || maxBytes < 1 || maxBytes > 4<<20 {
		return nil, ErrConfig
	}
	return readLogTailAt(s.dir, string(kind), maxBytes, s.store.uid)
}
