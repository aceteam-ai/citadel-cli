package heartbeat

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// markerFile is the on-disk basename of the cross-process heartbeat freshness
// marker. This package takes a configDir parameter rather than resolving one
// itself (same standalone convention as config.LoadEnergy/SaveEnergy), but
// unlike those, the caller must NOT pass platform.ConfigDir() here: that path
// is invoker-scoped (user-local unless root) and will silently diverge
// between the systemd-root `citadel work` that writes this marker and an
// interactive non-root `citadel status` that reads it -- exactly the
// duplicate-state-dir failure mode citadel-cli#383 fixed for tsnet state.
// Callers pass network.GetNodeConfigDir(), the same machine-convergent
// directory worklock's lock file already uses for this reason.
const markerFile = "heartbeat_marker.yaml"

// DefaultStaleAfter is the age past which a last-successful-heartbeat is
// treated as stale rather than merely "old". It is a fixed heuristic -- not
// tied to the live worker's configured --heartbeat interval, which a
// separate short-lived `citadel status` invocation has no way to read off a
// long-running `citadel work` process -- set to roughly 3x the default 30s
// interval so one or two missed cycles do not immediately cry wolf.
const DefaultStaleAfter = 3 * time.Minute

// Marker is the last-known state of the DURABLE heartbeat write (the
// XADD/StreamAdd call in redis.go/api.go's publishMessage), persisted so a
// separate `citadel status` invocation can report freshness for a heartbeat
// published by a long-running `citadel work` process (citadel-cli#726).
//
// This tracks the durable stream write only, not the best-effort pub/sub
// publish -- pub/sub freshness is already surfaced live by
// cmd/status.go:printPubSubInfo (#723), which reads the running worker's
// in-memory pubSubHealth over loopback. The two are deliberately distinct
// signals: "durable stream stale" (this marker) means the node is at risk of
// falling out of the fabric; "pub/sub failing, stream landing" (printPubSubInfo)
// means only live-dashboard freshness is degraded.
type Marker struct {
	// LastSuccessAt is when the durable stream write last succeeded. Zero
	// means never -- either this is a fresh install/config dir, or every
	// attempt so far has failed.
	LastSuccessAt time.Time `yaml:"last_success_at"`
	// LastAttemptAt is when the durable stream write was last attempted
	// (success or failure). Comparing it against LastSuccessAt distinguishes
	// "the worker stopped publishing entirely" (both stale) from "the worker
	// is publishing but every write fails" (LastAttemptAt fresh,
	// LastSuccessAt stale).
	LastAttemptAt time.Time `yaml:"last_attempt_at"`
	// ConsecutiveFailures counts durable-write failures since the last
	// success. Reset to 0 on success.
	ConsecutiveFailures int `yaml:"consecutive_failures,omitempty"`
	// LastError is the most recent durable-write failure's error string, if
	// any. Cleared on success.
	LastError string `yaml:"last_error,omitempty"`
}

// LoadMarker reads the heartbeat marker from configDir. A missing or
// unreadable file returns a zero-value Marker (never an error): the marker
// is a best-effort freshness signal, and a node that has never written one
// (fresh install, config dir predates this feature, or `citadel status` run
// with a different config dir than the live worker) must read as "unknown",
// not as an error.
func LoadMarker(configDir string) *Marker {
	m := &Marker{}
	data, err := os.ReadFile(filepath.Join(configDir, markerFile))
	if err != nil {
		return m
	}
	_ = yaml.Unmarshal(data, m)
	return m
}

// saveMarker writes m to configDir, creating the directory if needed.
func saveMarker(configDir string, m *Marker) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal heartbeat marker: %w", err)
	}
	return os.WriteFile(filepath.Join(configDir, markerFile), data, 0644)
}

// RecordSuccess updates the on-disk marker after a successful durable
// heartbeat write. Best-effort: publishMessage's return value is driven
// solely by the Redis/API write it already performed, so a failure here must
// never turn a successful heartbeat into a reported failure (reporting is
// not the apply path -- see #733's nodestate note for the same principle
// applied to the control-plane side). Callers therefore ignore the error.
func RecordSuccess(configDir string, at time.Time) error {
	m := LoadMarker(configDir)
	m.LastSuccessAt = at
	m.LastAttemptAt = at
	m.ConsecutiveFailures = 0
	m.LastError = ""
	return saveMarker(configDir, m)
}

// RecordFailure updates the on-disk marker after a failed durable heartbeat
// write. Best-effort for the same reason as RecordSuccess.
func RecordFailure(configDir string, at time.Time, writeErr error) error {
	m := LoadMarker(configDir)
	m.LastAttemptAt = at
	m.ConsecutiveFailures++
	if writeErr != nil {
		m.LastError = writeErr.Error()
	}
	return saveMarker(configDir, m)
}

// FreshnessState classifies a Marker for display.
type FreshnessState int

const (
	// FreshnessUnknown means no successful durable write has ever been
	// recorded in this config dir. Deliberately NOT treated as healthy: an
	// absent signal (worker never started, worker predates this feature, or
	// a wedged worker that never got past its first publish) must not read
	// as "fine" just because there is nothing to warn about.
	FreshnessUnknown FreshnessState = iota
	// FreshnessOK means the last successful durable write is within
	// staleAfter.
	FreshnessOK
	// FreshnessStale means the last successful durable write is older than
	// staleAfter -- the strong "heartbeat is not landing" signal #726 asks
	// for.
	FreshnessStale
)

// Freshness classifies m as of now, using staleAfter as the staleness
// threshold (pass DefaultStaleAfter unless a caller has a better bound). It
// returns the age of the last success alongside the state; age is only
// meaningful when the state is not FreshnessUnknown.
func Freshness(m *Marker, now time.Time, staleAfter time.Duration) (FreshnessState, time.Duration) {
	if m == nil || m.LastSuccessAt.IsZero() {
		return FreshnessUnknown, 0
	}
	age := now.Sub(m.LastSuccessAt)
	if age > staleAfter {
		return FreshnessStale, age
	}
	return FreshnessOK, age
}
