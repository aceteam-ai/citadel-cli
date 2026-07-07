// internal/platform/audio.go
//
// Meeting-audio capture: record exactly the sound a meeting-bot browser plays,
// with nothing else on the node mixed in (issue #5098, epic #5097 — the
// sovereign auto-join notetaker).
//
// The mechanism is the standard meeting-bot audio path, proven on a real node
// before this code was written:
//   1. Create a dedicated PulseAudio *null sink* (`pactl load-module
//      module-null-sink`). A null sink is a virtual output that plays to nowhere
//      but exposes a `.monitor` source carrying whatever was written to it.
//   2. Launch the browser with `PULSE_SINK=<sinkName>` in its environment so
//      libpulse routes THAT process's audio to our sink (per-process routing —
//      other node audio is untouched). See withPulseSink.
//   3. Record the sink's `.monitor` with ffmpeg to a WAV file in the node
//      workspace, where the whisper sidecar (read-only /workspace mount) can
//      then transcribe it.
//
// Why PulseAudio+ffmpeg and not in-browser capture: OS-level capture isolates
// the call audio from any other sound on the node, yields a clean on-disk file,
// and does not depend on tab focus or a fragile getDisplayMedia handshake.
//
// Deployment note: the node must have a running PulseAudio-protocol server
// (native pulseaudio OR pipewire-pulse) reachable by the citadel process — i.e.
// PULSE_SERVER / XDG_RUNTIME_DIR resolve to a live server. audioStackAvailable
// reports this so the node only advertises the `meeting` capability when capture
// can actually work.
package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// meetingSinkPrefix namespaces the null sinks this package creates so orphan
// reaping and diagnostics can find them, and so they never collide with a
// user's real sinks.
const meetingSinkPrefix = "citadel_meeting_"

// EnvPulseSink is the libpulse environment variable that pins the default sink
// for a client process's streams. Setting it on the browser's env is what routes
// the browser (and only the browser) into our null sink.
const EnvPulseSink = "PULSE_SINK"

// audioStackAvailable reports whether this node can capture meeting audio: the
// pactl and ffmpeg binaries exist AND a PulseAudio-protocol server is reachable
// (`pactl info` succeeds). Used both by capability detection (advertise the
// `meeting` tag only when true) and by the recorder as a pre-flight so failures
// surface as an actionable message rather than a mid-record ffmpeg crash.
func audioStackAvailable() bool {
	if !isCommandAvailable("pactl") || !isCommandAvailable("ffmpeg") {
		return false
	}
	// `pactl info` returns non-zero when no server is reachable. Bound it so a
	// hung server cannot wedge capability detection.
	cmd := exec.Command("pactl", "info")
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		return false
	}
}

// withPulseSink returns env with PULSE_SINK set to sinkName, replacing any
// inherited value, so the launched process routes its audio to exactly our sink.
// Pure (mirrors withDisplay) so the browser launcher can compose display + sink.
func withPulseSink(env []string, sinkName string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, EnvPulseSink+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, EnvPulseSink+"="+sinkName)
}

// buildAudioFFmpegArgs constructs the ffmpeg command line that records a pulse
// monitor source to a mono 16 kHz WAV — the format faster-whisper wants (16 kHz
// mono is whisper's native input, so we downsample once here rather than in the
// sidecar). Pure function so the arg set is unit-testable without ffmpeg.
func buildAudioFFmpegArgs(monitorSource, outPath string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "pulse", "-i", monitorSource,
		"-ac", "1", "-ar", "16000",
		"-y", outPath,
	}
}

// NullSinkRecorder owns one null sink and its ffmpeg recorder for a single
// meeting. Lifecycle: LoadSink (before the browser launches, so PULSE_SINK has a
// target) → the caller launches the browser with SinkName() in its env → Start
// (once joined) → Stop (finalize the WAV and unload the sink). Safe for
// concurrent use; the reaper goroutine owns the single ffmpeg Wait(), mirroring
// CobrowseManager's process handling.
type NullSinkRecorder struct {
	mu       sync.Mutex
	sinkName string
	moduleID string // pactl module index, needed to unload the sink
	outPath  string
	cmd      *exec.Cmd     // the ffmpeg recorder
	exited   chan struct{} // closed by the reaper when ffmpeg exits
}

// NewNullSinkRecorder creates a recorder bound to a unique sink name. The name is
// namespaced and caller-suffixed (e.g. a meeting id) so parallel meetings on one
// node never share a sink.
func NewNullSinkRecorder(idSuffix string) *NullSinkRecorder {
	return &NullSinkRecorder{sinkName: meetingSinkPrefix + sanitizeSinkSuffix(idSuffix)}
}

// sanitizeSinkSuffix keeps only characters PulseAudio accepts in a sink name so a
// meeting id (which may contain dashes) cannot produce an invalid module load.
func sanitizeSinkSuffix(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "session"
	}
	return b.String()
}

// SinkName is the null sink's name; put it in the browser env via withPulseSink.
func (r *NullSinkRecorder) SinkName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sinkName
}

// monitorSource is the source name ffmpeg records from.
func (r *NullSinkRecorder) monitorSource() string {
	return r.sinkName + ".monitor"
}

// LoadSink creates the null sink. Call before launching the browser so the
// browser's PULSE_SINK target exists at launch. Idempotent-ish: refuses if a sink
// is already loaded on this recorder.
func (r *NullSinkRecorder) LoadSink() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.moduleID != "" {
		return fmt.Errorf("null sink %q already loaded (module %s)", r.sinkName, r.moduleID)
	}
	if !audioStackAvailable() {
		return fmt.Errorf("audio capture unavailable: need pactl + ffmpeg and a running PulseAudio/pipewire-pulse server")
	}
	out, err := exec.Command("pactl", "load-module", "module-null-sink",
		"sink_name="+r.sinkName,
		"sink_properties=device.description="+r.sinkName,
	).Output()
	if err != nil {
		return fmt.Errorf("load null sink %q: %w", r.sinkName, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return fmt.Errorf("load null sink %q: pactl returned no module id", r.sinkName)
	}
	r.moduleID = id
	return nil
}

// Start begins recording the sink monitor to outPath. LoadSink must have run.
func (r *NullSinkRecorder) Start(outPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.moduleID == "" {
		return fmt.Errorf("cannot record: null sink not loaded (call LoadSink first)")
	}
	if r.cmd != nil {
		return fmt.Errorf("recorder already started")
	}
	cmd := exec.Command("ffmpeg", buildAudioFFmpegArgs(r.monitorSource(), outPath)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg recorder: %w", err)
	}
	r.cmd = cmd
	r.outPath = outPath
	exited := make(chan struct{})
	r.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return nil
}

// Stop finalizes the recording and unloads the sink. It sends ffmpeg SIGINT so it
// writes a valid WAV trailer, waits bounded for the reaper, then unloads the
// module. Safe to call once; safe when never started. Returns the recorded file
// path so the caller can hand it to transcription.
func (r *NullSinkRecorder) Stop() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	if r.cmd != nil && r.cmd.Process != nil {
		// SIGINT lets ffmpeg flush and finalize the WAV; a hard Kill can leave a
		// truncated file that whisper then fails to open.
		if err := r.cmd.Process.Signal(os.Interrupt); err != nil && !isProcessGoneErr(err) {
			firstErr = fmt.Errorf("signal ffmpeg: %w", err)
		}
		if r.exited != nil {
			select {
			case <-r.exited:
			case <-time.After(10 * time.Second):
				_ = r.cmd.Process.Kill()
			}
		}
	}
	r.cmd = nil
	r.exited = nil
	if r.moduleID != "" {
		if err := exec.Command("pactl", "unload-module", r.moduleID).Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("unload null sink %q (module %s): %w", r.sinkName, r.moduleID, err)
		}
		r.moduleID = ""
	}
	return r.outPath, firstErr
}
