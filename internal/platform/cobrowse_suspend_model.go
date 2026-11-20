package platform

// Pure, unwired planning primitives for #1821. No function here launches,
// closes, reads or writes a browser/profile/registry. The effectful manager
// must hold its action and lifecycle locks, durably commit the manifest before
// teardown, and independently validate the authenticated owner assertion.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	suspendManifestVersion        = 1
	maxSuspendManifestBytes       = 256 * 1024
	maxSuspendTabs                = 64
	maxSuspendURLBytes            = 8192
	maxSuspendTitleBytes          = 2048
	maxSuspendedPerOwner          = 5
	maxSuspendedNodeBytes   int64 = 8 * 1024 * 1024
	throwawaySuspendTTL           = 24 * time.Hour
	persistentSuspendTTL          = 7 * 24 * time.Hour
	suspendTombstoneTTL           = 7 * 24 * time.Hour
)

var suspendProfileName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

type suspendTab struct {
	URL     string  `json:"url"`
	Title   string  `json:"title,omitempty"`
	ScrollX float64 `json:"scroll_x"`
	ScrollY float64 `json:"scroll_y"`
	Active  bool    `json:"active,omitempty"`
}

// Full URLs can contain authentication codes. They belong only in a private
// node manifest, never status, events, errors or a job result. Persistent
// manifests must be sealed under cobrowse-suspend/v1/<id> before storage.
type suspendManifest struct {
	Version    int          `json:"version"`
	SessionID  string       `json:"session_id"`
	Generation uint64       `json:"generation"`
	Tabs       []suspendTab `json:"tabs"`
}

func (suspendManifest) String() string { return "[private browser tab manifest]" }

func (m suspendManifest) GoString() string { return m.String() }

// suspendMetadata is deliberately URL-free. The existing registry record
// remains the only lifecycle authority; the manifest is a separate bounded
// payload beside it, not a second session registry.
type suspendMetadata struct {
	// CaptureGeneration stays fixed while a resuming record advances its live
	// generation. Recovery must still find and decrypt the previous capture.
	CaptureGeneration uint64 `json:"capture_generation"`
	// ReopenRollback records the last positively stopped reopen attempt.
	// Only this receipt permits a suspended live generation above capture.
	ReopenRollback *suspendReopenRollback `json:"reopen_rollback,omitempty"`
	OwnerUserID    string                 `json:"owner_user_id"`
	ProfileName    string                 `json:"profile_name,omitempty"`
	SuspendedAt    string                 `json:"suspended_at"`
	ExpiresAt      string                 `json:"expires_at"`
	TabCount       int                    `json:"tab_count"`
	ManifestBytes  int64                  `json:"manifest_bytes"`
}

type suspendReopenRollback struct {
	Generation uint64 `json:"generation"`
	At         string `json:"at"`
}

func validSuspendGeneration(rec sessionRecord) bool {
	meta := rec.Suspension
	if meta == nil || meta.CaptureGeneration == 0 {
		return false
	}
	base := meta.CaptureGeneration
	if rollback := meta.ReopenRollback; rollback != nil {
		captured, e1 := time.Parse(time.RFC3339Nano, meta.SuspendedAt)
		at, e2 := time.Parse(time.RFC3339Nano, rollback.At)
		updated, e3 := time.Parse(time.RFC3339Nano, rec.UpdatedAt)
		if rollback.Generation <= base || e1 != nil || e2 != nil || e3 != nil ||
			at.IsZero() || at.Before(captured) || updated.Before(at) {
			return false
		}
		switch rec.State {
		case RecordSuspended:
			return rec.Generation == rollback.Generation && rec.UpdatedAt == rollback.At
		case "resuming":
			return rollback.Generation != math.MaxUint64 && rec.Generation == rollback.Generation+1
		default:
			return false
		}
	}
	if rec.State == "resuming" {
		return base != math.MaxUint64 && rec.Generation == base+1
	}
	return rec.Generation == base
}

func validSuspendOwner(owner string) bool {
	return owner != "" && len(owner) <= 128 && utf8.ValidString(owner) &&
		!strings.ContainsFunc(owner, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

func checkSuspendState(state string, humanDriving, ownerInitiated bool) error {
	switch state {
	case "pending_unlock":
		return errors.New("unlock or stop the browser session before suspending")
	case "launching", "resuming", "suspending":
		return errors.New("browser session is busy; retry later")
	case string(SessionRunning), string(SessionAttached):
		if humanDriving && !ownerInitiated {
			return errors.New("human is driving; only an owner-initiated request may suspend")
		}
		return nil
	default:
		return errors.New("only a running or attached browser session may suspend")
	}
}

func validateSuspendManifest(m suspendManifest, id string, generation uint64) error {
	if m.Version != suspendManifestVersion || !sessionIDPattern.MatchString(id) || len(id) > 64 ||
		m.SessionID != id || generation == 0 || m.Generation != generation {
		return errors.New("invalid browser tab manifest identity")
	}
	if len(m.Tabs) == 0 || len(m.Tabs) > maxSuspendTabs {
		return errors.New("browser tab manifest exceeds tab bounds")
	}
	active := 0
	for _, tab := range m.Tabs {
		if tab.URL == "" || len(tab.URL) > maxSuspendURLBytes || !utf8.ValidString(tab.URL) ||
			ValidateCobrowseURL(tab.URL) != nil {
			// The URL parser's error may contain the original URL. Never wrap it.
			return errors.New("invalid browser tab manifest URL")
		}
		if len(tab.Title) > maxSuspendTitleBytes || !utf8.ValidString(tab.Title) ||
			strings.ContainsFunc(tab.Title, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return errors.New("invalid browser tab manifest title")
		}
		for _, coordinate := range []float64{tab.ScrollX, tab.ScrollY} {
			if math.IsNaN(coordinate) || math.IsInf(coordinate, 0) || math.Abs(coordinate) > math.MaxInt32 {
				return errors.New("invalid browser tab manifest scroll position")
			}
		}
		if tab.Active {
			active++
		}
	}
	if active != 1 {
		return errors.New("browser tab manifest must name one active tab")
	}
	return nil
}

func encodeSuspendManifest(m suspendManifest) ([]byte, error) {
	if err := validateSuspendManifest(m, m.SessionID, m.Generation); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, errors.New("encode browser tab manifest")
	}
	if len(data) > maxSuspendManifestBytes {
		clear(data)
		return nil, errors.New("browser tab manifest exceeds byte bound")
	}
	return data, nil
}

func decodeSuspendManifest(data []byte, id string, generation uint64) (suspendManifest, error) {
	var m suspendManifest
	if len(data) == 0 || len(data) > maxSuspendManifestBytes || !utf8.Valid(data) {
		return m, errors.New("browser tab manifest exceeds byte bound or has invalid encoding")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return suspendManifest{}, errors.New("invalid browser tab manifest encoding")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return suspendManifest{}, errors.New("trailing browser tab manifest data")
	}
	if err := validateSuspendManifest(m, id, generation); err != nil {
		return suspendManifest{}, err
	}
	return m, nil
}

// planSuspendedRecord consumes a complete, validated capture and returns a
// URL-free registry snapshot with ALL live process fields cleared. Callers
// must account for stored ciphertext bytes (including encryption overhead),
// not merely the plaintext JSON size. Legacy unknown-owner sessions refuse.
func planSuspendedRecord(live sessionRecord, owner, profileName string, tabCount int, storedBytes int64,
	retained []sessionRecord, now time.Time) (sessionRecord, error) {
	if live.Version != sessionRecordVersion || live.State != RecordLive || live.Generation == 0 ||
		!sessionIDPattern.MatchString(live.ID) || len(live.ID) > 64 || !validSuspendOwner(owner) ||
		live.OwnerUserID != owner || now.IsZero() {
		return sessionRecord{}, errors.New("browser suspend requires a current owner-bound live record")
	}
	if live.Persistent != (profileName != "") || profileName != live.ProfileName ||
		(profileName != "" && !suspendProfileName.MatchString(profileName)) {
		return sessionRecord{}, errors.New("browser suspend profile binding mismatch")
	}
	if tabCount < 1 || tabCount > maxSuspendTabs || storedBytes <= 0 || storedBytes > maxSuspendManifestBytes+4096 {
		return sessionRecord{}, errors.New("browser suspend capture exceeds bounds")
	}
	var used int64
	count := 0
	for _, r := range retained {
		if r.State != RecordSuspended && r.State != "suspending" && r.State != "resuming" {
			continue
		}
		meta := r.Suspension
		if meta == nil || !validSuspendOwner(meta.OwnerUserID) || meta.OwnerUserID != r.OwnerUserID || meta.ManifestBytes <= 0 ||
			meta.ManifestBytes > maxSuspendManifestBytes+4096 || used > maxSuspendedNodeBytes-meta.ManifestBytes {
			return sessionRecord{}, errors.New("cannot establish bounded suspended-session accounting")
		}
		if r.ID == live.ID {
			return sessionRecord{}, errors.New("browser session already has suspended storage")
		}
		used += meta.ManifestBytes
		if meta.OwnerUserID == owner {
			count++
		}
	}
	if count >= maxSuspendedPerOwner || used > maxSuspendedNodeBytes-storedBytes {
		return sessionRecord{}, errors.New("suspended browser capacity reached; discard one first")
	}
	ttl := throwawaySuspendTTL
	if live.Persistent {
		ttl = persistentSuspendTTL
	}
	now = now.UTC()
	return sessionRecord{
		Version: sessionRecordVersion, ID: live.ID, Persistent: live.Persistent,
		State: RecordSuspended, Generation: live.Generation, Handoff: live.Handoff,
		Tracking: live.Tracking, StartedAt: live.StartedAt, OwnerUserID: owner, ProfileName: profileName,
		Suspension: &suspendMetadata{CaptureGeneration: live.Generation, OwnerUserID: owner, ProfileName: profileName,
			SuspendedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(ttl).Format(time.RFC3339Nano),
			TabCount: tabCount, ManifestBytes: storedBytes},
	}, nil
}

func suspendExpiry(rec sessionRecord) (time.Time, error) {
	meta := rec.Suspension
	if meta == nil || !validSuspendGeneration(rec) || meta.TabCount < 1 || meta.TabCount > maxSuspendTabs ||
		meta.ManifestBytes <= 0 || meta.ManifestBytes > maxSuspendManifestBytes+4096 {
		return time.Time{}, errors.New("invalid suspended browser metadata")
	}
	return suspendCaptureExpiry(rec)
}

// Shared immutable capture identity and TTL validation. Live-retained records
// additionally require valid generation/counters; expired tombstones instead
// require a validated terminal receipt and zero counters.
func suspendCaptureExpiry(rec sessionRecord) (time.Time, error) {
	meta := rec.Suspension
	if rec.Version != sessionRecordVersion || rec.Generation == 0 || !sessionIDPattern.MatchString(rec.ID) || len(rec.ID) > 64 ||
		meta == nil || meta.OwnerUserID != rec.OwnerUserID || !validSuspendOwner(meta.OwnerUserID) ||
		meta.ProfileName != rec.ProfileName || rec.Persistent != (rec.ProfileName != "") ||
		(rec.Persistent && !suspendProfileName.MatchString(rec.ProfileName)) {
		return time.Time{}, errors.New("invalid suspended browser metadata")
	}
	start, e1 := time.Parse(time.RFC3339Nano, meta.SuspendedAt)
	expiry, e2 := time.Parse(time.RFC3339Nano, meta.ExpiresAt)
	ttl := throwawaySuspendTTL
	if rec.Persistent {
		ttl = persistentSuspendTTL
	}
	if e1 != nil || e2 != nil || start.IsZero() || !expiry.Equal(start.Add(ttl)) {
		return time.Time{}, errors.New("invalid suspended browser expiry")
	}
	return expiry, nil
}

// planReopenRecord does not consume an unlock credential. It takes only the
// result of an independently successful human unlock. The manager still has
// to validate the profile handle/name, live slot and decrypted manifest before
// launching. Errors never downgrade a persistent session to throwaway.
func planReopenRecord(rec sessionRecord, owner string, unlocked bool, now time.Time) (sessionRecord, error) {
	if rec.Version != sessionRecordVersion || rec.State != RecordSuspended ||
		!validSuspendOwner(owner) || rec.OwnerUserID != owner || rec.Generation == 0 ||
		rec.Generation == math.MaxUint64 || !sessionIDPattern.MatchString(rec.ID) || len(rec.ID) > 64 || now.IsZero() {
		return sessionRecord{}, errors.New("reopen requires an owner-bound suspended browser session")
	}
	expiry, err := suspendExpiry(rec)
	if err != nil {
		return sessionRecord{}, err
	}
	if !now.Before(expiry) {
		return sessionRecord{}, errors.New("suspended browser session has expired")
	}
	if rec.Persistent && !unlocked {
		return sessionRecord{}, errors.New("human unlock is required to reopen saved browser logins")
	}
	rec.State = "resuming"
	rec.Generation++
	return rec, nil
}

// planSuspendExpiry describes a sweep transition, not a completed deletion.
// The effectful sweep must remove the manifest before committing the returned
// tombstone (and must surface either failure). The persistent profile is NOT
// removed. Tombstones retain the original expiry, never a refreshed timestamp.
func planSuspendExpiry(rec sessionRecord, now time.Time) (sessionRecord, bool, error) {
	if rec.State != RecordSuspended || now.IsZero() {
		return rec, false, nil
	}
	expiry, err := suspendExpiry(rec)
	if err != nil {
		return rec, false, err
	}
	if now.Before(expiry) {
		return rec, false, nil
	}
	rec.State, rec.EndedReason, rec.EndedAt = RecordExpired, "suspend_expired", expiry.UTC().Format(time.RFC3339Nano)
	meta := *rec.Suspension
	meta.ManifestBytes, meta.TabCount = 0, 0
	rec.Suspension = &meta
	return rec, true, nil
}

func suspendTombstoneDue(rec sessionRecord, now time.Time) bool {
	if rec.State != RecordExpired || rec.EndedReason != "suspend_expired" || now.IsZero() {
		return false
	}
	expiry, err := time.Parse(time.RFC3339Nano, rec.EndedAt)
	return err == nil && !now.Before(expiry.Add(suspendTombstoneTTL))
}

// Use only in diagnostics that must not accidentally format private tab URLs.
func suspendManifestSummary(m suspendManifest) string {
	return fmt.Sprintf("browser tab manifest: %d tabs", len(m.Tabs))
}
