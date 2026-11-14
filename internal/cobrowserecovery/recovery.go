// Package cobrowserecovery binds co-browse execution to a durable, scoped job ID.
// It stores no request body, URL, profile name, PIN, or browser content.
package cobrowserecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type State string

const (
	NotFound      State = "not_found"
	Launching     State = "launching"
	Running       State = "running"
	StopRequested State = "stop_requested"
	Stopped       State = "stopped"
	Ended         State = "ended"
	Failed        State = "failed"
	Unknown       State = "unknown"
	MaxRecords          = 8192
	Retention           = 48 * time.Hour
)

var (
	ErrUnavailable = errors.New("co-browse recovery journal unavailable; execution refused")
	ErrConflict    = errors.New("co-browse recovery scope or request conflict")
)

// Scope is supplied by the authenticated producer, and bound to the actual node
// stream by the adapter. NodeOrg is deliberately NOT the billing caller org.
type Scope struct {
	JobID string `json:"start_job_id"`
	Node  string `json:"target_node"`
	Org   string `json:"caller_org_id"`
	Owner string `json:"owner_user_id"`
}

func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
func (s Scope) valid() bool {
	return canonicalID(s.JobID) && canonicalID(s.Org) && canonicalID(s.Owner) && len(s.Node) > 0 && len(s.Node) <= 128 && !strings.ContainsAny(s.Node, ":/\\ \t\r\n")
}

// Digest uses an unambiguous ordered encoding, including the URL query but never
// retaining it. PIN is deliberately absent: a failed PIN consumes this attempt.
func Digest(rawURL, profile string) (string, error) {
	if len(rawURL) > 16384 || len(profile) > 128 {
		return "", ErrConflict
	}
	if rawURL == "" {
		rawURL = "about:blank"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ErrConflict
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	b, _ := json.Marshal([2]string{u.String(), profile})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type Record struct {
	Scope
	Digest     string    `json:"request_digest,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	State      State     `json:"state"`
	TerminalAt time.Time `json:"terminal_at,omitempty"`
}

type Result struct {
	JobID     string `json:"start_job_id"`
	State     State  `json:"state"`
	SessionID string `json:"session_id,omitempty"`
}

func (r Record) result() Result { return Result{r.JobID, r.State, r.SessionID} }
func terminal(s State) bool     { return s == Stopped || s == Ended || s == Failed }
func validRecord(r Record) bool {
	if !r.Scope.valid() {
		return false
	}
	switch r.State {
	case Launching, Running, StopRequested, Stopped, Ended, Failed, Unknown:
	default:
		return false
	}
	if r.SessionID != "" && r.SessionID != "cb-"+strings.ReplaceAll(r.JobID, "-", "") {
		return false
	}
	if r.Digest != "" {
		b, e := hex.DecodeString(r.Digest)
		if e != nil || len(b) != sha256.Size {
			return false
		}
	}
	if terminal(r.State) {
		return !r.TerminalAt.IsZero()
	}
	return r.TerminalAt.IsZero() && r.SessionID != "" && r.Digest != ""
}

// Transaction holds the cross-process journal lock until Close. Launch/stop also
// run under this lock. Contention refuses rather than overlapping a launch or
// falsely confirming a stop; callers retain quota and retry the exact job ID.
type Transaction interface {
	Records() map[string]Record
	Save() error
	Close()
}
type Store interface{ Begin() (Transaction, error) }

// Ops owns process identity and exit evidence. Start reports whether browser
// launch was attempted even on error. Observe must return Unknown for absence:
// absence after a crash is not proof that an earlier launch never happened.
type Ops interface {
	Start(sessionID string) (attempted bool, err error)
	Observe(sessionID string) State
	Stop(sessionID string) (confirmed bool, err error)
}

type Engine struct {
	Store Store
	Now   func() time.Time
}

func (e Engine) Do(action string, scope Scope, digest string, ops Ops) (Result, error) {
	if !scope.valid() || ops == nil || e.Store == nil {
		return Result{}, ErrConflict
	}
	if action != "start" && action != "job_status" && action != "stop_by_job_id" {
		return Result{}, ErrConflict
	}
	if action == "start" {
		b, err := hex.DecodeString(digest)
		if err != nil || len(b) != sha256.Size {
			return Result{}, ErrConflict
		}
	}
	tx, err := e.Store.Begin()
	if err != nil {
		return Result{}, ErrUnavailable
	}
	defer tx.Close()
	records := tx.Records()
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	at := now().UTC()
	r, exists := records[scope.JobID]
	if exists && (r.Scope != scope || (action == "start" && r.Digest != "" && r.Digest != digest)) {
		return Result{}, ErrConflict
	}
	if !exists {
		if action == "job_status" {
			return Result{JobID: scope.JobID, State: NotFound}, nil
		}
		// Never evict an active/unknown record, or a tombstone inside its retention.
		for id, old := range records {
			if terminal(old.State) && !at.Before(old.TerminalAt) && at.Sub(old.TerminalAt) >= Retention {
				delete(records, id)
			}
		}
		if len(records) >= MaxRecords {
			return Result{}, ErrUnavailable
		}
		r = Record{Scope: scope}
		if action == "stop_by_job_id" {
			r.State = Stopped
			r.TerminalAt = now().UTC()
			records[scope.JobID] = r
			if tx.Save() != nil {
				return Result{}, ErrUnavailable
			}
			return r.result(), nil
		}
		r.SessionID = "cb-" + strings.ReplaceAll(scope.JobID, "-", "")
		r.State = Launching
		r.Digest = digest
		records[scope.JobID] = r
		// The sole execution boundary: successful file AND directory fsync first.
		if tx.Save() != nil {
			return Result{}, ErrUnavailable
		}
		attempted, startErr := ops.Start(r.SessionID)
		if startErr != nil {
			if !attempted {
				r.State = Failed
				r.TerminalAt = now().UTC()
			} else {
				r.State = Unknown
			}
		} else {
			r.State = Running
		}
		records[scope.JobID] = r
		if tx.Save() != nil {
			return Result{}, ErrUnavailable
		}
		if startErr != nil {
			return r.result(), fmt.Errorf("co-browse start did not complete; recover by start_job_id")
		}
		return r.result(), nil
	}
	if terminal(r.State) {
		return r.result(), nil
	}
	if action == "stop_by_job_id" || r.State == StopRequested {
		r.State = StopRequested
		records[scope.JobID] = r
		// Persist the stop intent before touching the browser.
		if tx.Save() != nil {
			return Result{}, ErrUnavailable
		}
		confirmed, stopErr := ops.Stop(r.SessionID)
		if confirmed && stopErr == nil {
			r.State = Stopped
			r.TerminalAt = now().UTC()
		}
		records[scope.JobID] = r
		if tx.Save() != nil {
			return Result{}, ErrUnavailable
		}
		if stopErr != nil {
			return r.result(), fmt.Errorf("co-browse stop unconfirmed; retry start_job_id")
		}
		return r.result(), nil
	}
	switch ops.Observe(r.SessionID) {
	case Running:
		r.State = Running
	case Ended:
		// A crashed browser still owns a display/profile until its manager tears
		// down that custody. Do not strand it behind a terminal journal record.
		if confirmed, stopErr := ops.Stop(r.SessionID); confirmed && stopErr == nil {
			r.State = Ended
			r.TerminalAt = now().UTC()
		} else {
			r.State = Unknown
		}
	default:
		r.State = Unknown
	}
	records[scope.JobID] = r
	if tx.Save() != nil {
		return Result{}, ErrUnavailable
	}
	return r.result(), nil
}
