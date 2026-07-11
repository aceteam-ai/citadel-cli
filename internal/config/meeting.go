package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Meeting controls the auto-join meeting-notetaker capability. It follows the
// same opt-out (default-true) model as Telemetry: a node that has the audio +
// browser dependencies advertises the `meeting` tag and registers the
// MEETING_JOIN handler out of the box, and the operator may turn it off. This
// is the house convention for node capabilities — capabilities are config
// toggles, default opted-in, with a Control Center opt-out and a programmatic
// path (APPLY_DEVICE_CONFIG) that writes this same persisted value.
type Meeting struct {
	// MeetingEnabled gates the `meeting` capability. When true (and the node's
	// audio/browser deps are present) the node advertises the `meeting` tag and
	// registers the MEETING_JOIN handler. When false the node stays out of the
	// meeting queue regardless of deps. Defaults to true (opt-out) so meeting
	// nodes work out of the box; the Control Center settings pane discloses the
	// default-on behavior and offers the toggle.
	MeetingEnabled bool `yaml:"meeting_enabled" json:"meeting_enabled"`

	// StreamingEnabled gates the DURING-CALL rolling transcription + in-call
	// command monitor (issue #5435). When false, the bot behaves exactly like
	// the shipped batch notetaker: join, record, transcribe once at the end.
	// When true, the recording is additionally transcribed in a rolling window
	// as the call runs so the bot can react to spoken/typed `/ace` commands
	// live. Default-on (opt-out), same house convention as MeetingEnabled; the
	// interactive layer is built to degrade gracefully (log + continue) so a
	// stale live selector can never regress the batch pipeline.
	StreamingEnabled bool `yaml:"streaming_enabled" json:"streaming_enabled"`

	// StreamingIntervalSeconds is how often (during the call) the growing
	// recording is re-transcribed to surface new transcript segments. Smaller
	// values lower command-detection latency at the cost of more whisper passes;
	// the v1 strategy re-transcribes the whole wav-so-far, so passes get slower
	// as a long call grows (see meeting_transcribe_rolling.go). Must be positive.
	StreamingIntervalSeconds int `yaml:"streaming_interval_seconds" json:"streaming_interval_seconds"`

	// StreamingWindowSeconds is the trailing "not yet stable" margin. Whole-file
	// re-transcription revises the most recent audio (segment text and start
	// times shift as more context arrives), so a segment is only emitted
	// downstream once its end is older than (current tail - window). This holds
	// back the churning tail to avoid emitting — and acting on — a partial or
	// hallucinated segment that the next pass rewrites. Must be positive.
	StreamingWindowSeconds int `yaml:"streaming_window_seconds" json:"streaming_window_seconds"`
}

// StreamingInterval returns the rolling-transcription cadence as a Duration,
// falling back to the default when a persisted config carries a non-positive
// value (e.g. a hand-edited or truncated meeting.yaml). Keeping the clamp here
// means the consumer never has to defend against a zero ticker interval.
func (m *Meeting) StreamingInterval() time.Duration {
	if m.StreamingIntervalSeconds <= 0 {
		return time.Duration(defaultStreamingIntervalSeconds) * time.Second
	}
	return time.Duration(m.StreamingIntervalSeconds) * time.Second
}

// StreamingWindow returns the trailing stability margin as a Duration, with the
// same non-positive fallback rationale as StreamingInterval.
func (m *Meeting) StreamingWindow() time.Duration {
	if m.StreamingWindowSeconds <= 0 {
		return time.Duration(defaultStreamingWindowSeconds) * time.Second
	}
	return time.Duration(m.StreamingWindowSeconds) * time.Second
}

const meetingFile = "meeting.yaml"

// Streaming defaults. Interval trades command latency against whisper load;
// 15s is a middle ground on CPU "base". Window must comfortably exceed a
// typical whisper segment (~5-10s) so the churning tail stabilizes before we
// emit it.
const (
	defaultStreamingIntervalSeconds = 15
	defaultStreamingWindowSeconds   = 10
)

// DefaultMeeting returns a Meeting struct with the capability enabled.
// Default-on is intentional: it matches the house convention that capabilities
// are opt-out, so a dep-capable node joins meetings without extra setup.
func DefaultMeeting() *Meeting {
	return &Meeting{
		MeetingEnabled:           true,
		StreamingEnabled:         true,
		StreamingIntervalSeconds: defaultStreamingIntervalSeconds,
		StreamingWindowSeconds:   defaultStreamingWindowSeconds,
	}
}

// LoadMeeting reads meeting settings from the config directory.
// If the file doesn't exist, returns defaults (enabled).
// Partial files preserve defaults for absent keys (unmarshal into a
// pre-initialized struct), mirroring LoadTelemetry.
func LoadMeeting(configDir string) *Meeting {
	m := DefaultMeeting()

	data, err := os.ReadFile(filepath.Join(configDir, meetingFile))
	if err != nil {
		return m
	}

	// yaml.Unmarshal only overwrites keys present in the file, so an absent
	// key keeps its default (true) value.
	_ = yaml.Unmarshal(data, m)
	return m
}

// SaveMeeting writes meeting settings to the config directory.
// The Control Center settings pane calls this when the operator toggles the
// flag, and the APPLY_DEVICE_CONFIG handler calls it when the platform pushes
// an explicit value.
func SaveMeeting(configDir string, m *Meeting) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal meeting: %w", err)
	}

	return os.WriteFile(filepath.Join(configDir, meetingFile), data, 0644)
}
