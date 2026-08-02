// internal/jobs/huddle_converse.go
//
// The resident realtime CONVERSE bridge for HUDDLE_JOIN (aceteam#7079/#7081).
// Once HUDDLE_JOIN reaches `joined`, this turns the joined-but-silent bot into a
// live two-way voice participant by bridging three legs:
//
//	HEAR   room audio (meetingd GET /sessions/{id}/capture/pcm, raw s16le 24 kHz)
//	       -> WS `input_audio_buffer.append` (base64 PCM16) to the realtime engine
//	THINK  the AceTeam realtime engine (OpenAI Realtime behind an AceTeam WS proxy)
//	       does STT + LLM + TTS with SERVER-SIDE VAD (we never force turns)
//	SPEAK  engine `response.output_audio.delta` (base64 PCM16 24 kHz) -> meetingd
//	       POST /mic/play/pcm -> the container virtual mic -> the huddle hears it
//
// It is ADDITIVE and GATED: HUDDLE_JOIN only starts it when the payload carries
// `converse:true`. With converse off (the default) the handler behaves EXACTLY like
// the #667 join+confirm wave -- no capture, no WS, no mic play.
//
// --- design notes ------------------------------------------------------------
//
// Standalone + injectable, like internal/mesh: the bridge talks to a `realtimeConn`
// (a minimal duplex WS seam) and a `converseMedia` (capture + speak), so the whole
// loop is unit-testable against a mock WS + mock media with no live container,
// mesh, or engine. The production wiring lives in huddle_join.go.
//
// Echo hygiene: the room capture is the per-session sink `citadel_meeting_<sid>`'s
// monitor -- the OTHER participants' mixed audio. The bot's own TTS is published on
// a DIFFERENT sink (`citadel_mic`), and WebRTC does not echo a peer's own mic back
// to it, so the capture is echo-free BY CONSTRUCTION. As belt-and-suspenders (and
// for any node where the two ever merge) the bridge GATES the HEAR->engine stream
// while it is speaking (default on), with a short hangover after playback and an
// `input_audio_buffer.clear` on gate release so no half-buffered self-audio leaks
// in. Barge-in: on the engine's `input_audio_buffer.speech_started` we drop queued
// TTS so the agent stops talking over a human (current in-flight chunk still plays
// -- bounded by the coalesce window; true mid-clip stop is a documented follow-up).
package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Realtime engine wire format: PCM16 mono @ 24 kHz (aceteam
// buildSessionConfig.PCM16_FORMAT). Both legs use it, so with the capture endpoint
// serving 24 kHz there is no resample on the hot path.
const (
	converseEngineRate     = 24000
	converseEngineChannels = 1
	bytesPerSample         = 2 // s16le
)

// Client->server WS message types the bridge sends (aceteam types/realtimeWs.ts
// zRealtimeClientPayloadSchema). With server VAD on we send ONLY append + clear;
// commit / response.create would force turns and are deliberately never sent.
const (
	msgInputAudioAppend = "input_audio_buffer.append"
	msgInputAudioClear  = "input_audio_buffer.clear"
)

// Server->client WS event types the bridge acts on (aceteam app/api/realtime).
const (
	evtAudioDelta    = "response.output_audio.delta"
	evtAudioDone     = "response.output_audio.done"
	evtSpeechStarted = "input_audio_buffer.speech_started"
)

// realtimeConn is the minimal duplex WebSocket surface the bridge needs. The
// production impl (gorillaRealtimeConn) wraps gorilla/websocket; tests inject a
// mock. Send/Recv are each called from a single goroutine (HEAR/speaker send;
// recv reads), matching gorilla's one-reader/one-writer rule.
type realtimeConn interface {
	Send(payload []byte) error
	Recv() ([]byte, error)
	Close() error
}

// converseMedia is the container-media surface the bridge needs: a rolling raw-PCM
// capture of the room (HEAR) and a raw-PCM inject into the virtual mic (SPEAK).
// *containerMedia satisfies it.
type converseMedia interface {
	CaptureStream(ctx context.Context, rate, channels int) (io.ReadCloser, error)
	SpeakPCM(pcm []byte, rate, channels int) error
}

// stateCheckFunc reports whether the bridge should stop because the call ended.
// ended=true stops the loop; reason is for the log/result. huddle_join.go wires
// this to a poll of window.__huddleBotState (left/error). A nil/never-ending check
// keeps the bridge resident until ctx cancels (the worker deadline / node stop).
type stateCheckFunc func() (ended bool, reason string)

// converseStats is the additive per-run summary folded into the HUDDLE_JOIN
// result JSON so the backend's existing `status:"joined"` parse is unaffected.
type converseStats struct {
	AppendedFrames int    `json:"appended_frames"`
	AudioDeltas    int    `json:"audio_deltas"`
	SpokenChunks   int    `json:"spoken_chunks"`
	GatedFrames    int    `json:"gated_frames"`
	Barges         int    `json:"barge_ins"`
	DurationMS     int64  `json:"duration_ms"`
	StopReason     string `json:"stop_reason"`
}

// converseConfig tunes the bridge; zero values fall back to package defaults so
// callers set only what they need and tests set milliseconds.
type converseConfig struct {
	// EngineRate/EngineChannels default to 24000/1 (the engine's PCM16 format).
	EngineRate     int
	EngineChannels int
	// CaptureRate is the rate CaptureStream is asked to serve. When it differs
	// from EngineRate the HEAR path linearly resamples each frame. Default ==
	// EngineRate (no resample).
	CaptureRate int
	// FrameDuration is the size of one HEAR->append frame (default 20ms).
	FrameDuration time.Duration
	// SpeakCoalesce is how long the speaker gathers newly-arrived deltas into one
	// pacat play before posting, to avoid one subprocess spawn per delta (default
	// 200ms).
	SpeakCoalesce time.Duration
	// SpeakHangover holds the speaking gate closed this long after the last chunk
	// plays, before reopening HEAR (default 250ms).
	SpeakHangover time.Duration
	// GateWhileSpeaking drops HEAR frames while the bot is speaking (default on).
	// Disable to allow full-duplex barge-in on an echo-free node.
	GateWhileSpeaking *bool
	// MicBusyBackoff is the retry pause on a 409 from SpeakPCM (default 50ms).
	MicBusyBackoff time.Duration
	// StatePollInterval is how often stateCheck is sampled (default 3s).
	StatePollInterval time.Duration
}

// converseBridge is one resident conversation. Construct with newConverseBridge
// and drive with Run. It owns no cleanup of conn/media -- the caller does.
type converseBridge struct {
	conn  realtimeConn
	media converseMedia
	state stateCheckFunc
	log   func(level, format string, args ...any)
	cfg   converseConfig

	speaking atomic.Bool
	speakCh  chan []byte

	// stats is mutated only from the recv/hear/speaker goroutines; guarded so the
	// final read in Run is race-free.
	statsMu sync.Mutex
	stats   converseStats
}

func newConverseBridge(conn realtimeConn, media converseMedia, state stateCheckFunc, log func(string, string, ...any), cfg converseConfig) *converseBridge {
	if cfg.EngineRate <= 0 {
		cfg.EngineRate = converseEngineRate
	}
	if cfg.EngineChannels <= 0 {
		cfg.EngineChannels = converseEngineChannels
	}
	if cfg.CaptureRate <= 0 {
		cfg.CaptureRate = cfg.EngineRate
	}
	if cfg.FrameDuration <= 0 {
		cfg.FrameDuration = 20 * time.Millisecond
	}
	if cfg.SpeakCoalesce <= 0 {
		cfg.SpeakCoalesce = 200 * time.Millisecond
	}
	if cfg.SpeakHangover <= 0 {
		cfg.SpeakHangover = 250 * time.Millisecond
	}
	if cfg.MicBusyBackoff <= 0 {
		cfg.MicBusyBackoff = 50 * time.Millisecond
	}
	if cfg.StatePollInterval <= 0 {
		cfg.StatePollInterval = 3 * time.Second
	}
	if cfg.GateWhileSpeaking == nil {
		on := true
		cfg.GateWhileSpeaking = &on
	}
	if log == nil {
		log = func(string, string, ...any) {}
	}
	return &converseBridge{
		conn:    conn,
		media:   media,
		state:   state,
		log:     log,
		cfg:     cfg,
		speakCh: make(chan []byte, 64),
	}
}

// Run drives the bridge until the call ends (stateCheck), the parent ctx cancels,
// or the WS closes, then returns the run stats. It fans out HEAR, RECV, SPEAK and a
// state supervisor; the FIRST to finish cancels the rest via a derived context.
func (b *converseBridge) Run(parent context.Context) (converseStats, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	start := time.Now()

	var stopReason atomic.Value // string
	stop := func(reason string) {
		stopReason.CompareAndSwap(nil, reason)
		cancel()
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); b.hearLoop(ctx, stop) }()
	go func() { defer wg.Done(); b.recvLoop(ctx, stop) }()
	go func() { defer wg.Done(); b.speakerLoop(ctx) }()

	// Supervisor: watch for parent cancel + the call ending. Runs on Run's
	// goroutine so Run blocks here until a terminal condition.
	b.superviseLoop(ctx, stop)

	// A terminal condition fired: cancel everything and drain the workers. Closing
	// the WS unblocks recvLoop's Recv; the media capture body is closed by hearLoop
	// on ctx cancel.
	cancel()
	_ = b.conn.Close()
	wg.Wait()

	reason, _ := stopReason.Load().(string)
	if reason == "" {
		reason = "stopped"
	}
	b.statsMu.Lock()
	out := b.stats
	b.statsMu.Unlock()
	out.DurationMS = time.Since(start).Milliseconds()
	out.StopReason = reason
	return out, nil
}

// superviseLoop blocks until parent ctx cancels or stateCheck reports the call
// ended, then calls stop with a reason.
func (b *converseBridge) superviseLoop(ctx context.Context, stop func(string)) {
	t := time.NewTicker(b.cfg.StatePollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			stop("context cancelled")
			return
		case <-t.C:
			if b.state == nil {
				continue
			}
			if ended, reason := b.state(); ended {
				if reason == "" {
					reason = "call ended"
				}
				b.log("info", "     - converse: stopping (%s)", reason)
				stop(reason)
				return
			}
		}
	}
}

// hearLoop streams room PCM from the capture endpoint and forwards it as
// `input_audio_buffer.append` frames. It fills fixed-size frames, resamples when
// CaptureRate != EngineRate, and (when the gate is on) drops frames while speaking.
func (b *converseBridge) hearLoop(ctx context.Context, stop func(string)) {
	rc, err := b.media.CaptureStream(ctx, b.cfg.CaptureRate, b.cfg.EngineChannels)
	if err != nil {
		b.log("warn", "     - converse: capture stream failed: %v", err)
		stop("capture stream error")
		return
	}
	// Close the body when ctx cancels so a blocked Read returns.
	go func() {
		<-ctx.Done()
		_ = rc.Close()
	}()
	defer rc.Close()

	frameBytes := pcmFrameBytes(b.cfg.CaptureRate, b.cfg.EngineChannels, b.cfg.FrameDuration)
	buf := make([]byte, frameBytes)
	gate := *b.cfg.GateWhileSpeaking

	for {
		if ctx.Err() != nil {
			return
		}
		n, err := readFullFrame(rc, buf)
		// Drop a trailing partial SAMPLE (a short final read at stream teardown can
		// leave an odd byte count, which would base64 into half an s16le sample the
		// engine may reject).
		n -= n % bytesPerSample
		if n > 0 {
			frame := buf[:n]
			if b.cfg.CaptureRate != b.cfg.EngineRate {
				frame = resamplePCM16(frame, b.cfg.CaptureRate, b.cfg.EngineRate, b.cfg.EngineChannels)
			}
			if gate && b.speaking.Load() {
				b.bumpGated()
			} else if sendErr := b.sendAppend(frame); sendErr != nil {
				b.log("warn", "     - converse: append send failed: %v", sendErr)
				stop("ws send error")
				return
			}
		}
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, io.EOF) {
				b.log("warn", "     - converse: capture read ended: %v", err)
			}
			stop("capture ended")
			return
		}
	}
}

// sendAppend base64-encodes a PCM frame and sends it as input_audio_buffer.append.
func (b *converseBridge) sendAppend(frame []byte) error {
	msg, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}{Type: msgInputAudioAppend, Audio: base64.StdEncoding.EncodeToString(frame)})
	if err := b.conn.Send(msg); err != nil {
		return err
	}
	b.statsMu.Lock()
	b.stats.AppendedFrames++
	b.statsMu.Unlock()
	return nil
}

func (b *converseBridge) sendClear() error {
	msg, _ := json.Marshal(struct {
		Type string `json:"type"`
	}{Type: msgInputAudioClear})
	return b.conn.Send(msg)
}

// recvLoop reads engine events: audio deltas become queued speak chunks; a
// speech_started drops queued TTS (barge-in). A Recv error ends the run.
func (b *converseBridge) recvLoop(ctx context.Context, stop func(string)) {
	for {
		raw, err := b.conn.Recv()
		if err != nil {
			if ctx.Err() == nil {
				b.log("warn", "     - converse: ws recv ended: %v", err)
			}
			stop("ws closed")
			return
		}
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		switch ev.Type {
		case evtAudioDelta:
			pcm, decErr := base64.StdEncoding.DecodeString(ev.Delta)
			if decErr != nil || len(pcm) == 0 {
				continue
			}
			b.statsMu.Lock()
			b.stats.AudioDeltas++
			b.statsMu.Unlock()
			select {
			case b.speakCh <- pcm:
			case <-ctx.Done():
				return
			}
		case evtSpeechStarted:
			// Barge-in: the human started talking; drop pending TTS so the agent
			// yields the floor.
			if n := b.drainSpeakCh(); n > 0 {
				b.statsMu.Lock()
				b.stats.Barges++
				b.statsMu.Unlock()
				b.log("info", "     - converse: barge-in, dropped %d queued TTS chunk(s)", n)
			}
		case evtAudioDone:
			// Nothing to force: the speaker releases the gate on idle.
		}
	}
}

// speakerLoop drains queued PCM deltas, coalesces the burst into one play to avoid
// a pacat spawn per delta, injects it into the virtual mic, and releases the
// speaking gate (with a hangover + clear) once the queue goes idle.
func (b *converseBridge) speakerLoop(ctx context.Context) {
	idle := time.NewTimer(time.Hour)
	if !idle.Stop() {
		<-idle.C
	}
	releasePending := false
	for {
		select {
		case <-ctx.Done():
			return
		case chunk := <-b.speakCh:
			b.speaking.Store(true)
			releasePending = false
			buf := b.coalesce(ctx, chunk)
			b.play(ctx, buf)
			releasePending = true
			resetTimer(idle, b.cfg.SpeakHangover)
		case <-idle.C:
			if releasePending {
				b.speaking.Store(false)
				_ = b.sendClear()
				releasePending = false
			}
		}
	}
}

// coalesce appends any deltas already queued (up to SpeakCoalesce) to the first
// chunk so a burst plays as one clip.
func (b *converseBridge) coalesce(ctx context.Context, first []byte) []byte {
	buf := append([]byte(nil), first...)
	deadline := time.NewTimer(b.cfg.SpeakCoalesce)
	defer deadline.Stop()
	for {
		select {
		case more := <-b.speakCh:
			buf = append(buf, more...)
		case <-deadline.C:
			return buf
		case <-ctx.Done():
			return buf
		}
	}
}

// play injects PCM into the virtual mic, retrying once on a mic-busy 409.
func (b *converseBridge) play(ctx context.Context, pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	err := b.media.SpeakPCM(pcm, b.cfg.EngineRate, b.cfg.EngineChannels)
	if errors.Is(err, errMicBusy) {
		select {
		case <-time.After(b.cfg.MicBusyBackoff):
		case <-ctx.Done():
			return
		}
		err = b.media.SpeakPCM(pcm, b.cfg.EngineRate, b.cfg.EngineChannels)
	}
	if err != nil {
		b.log("warn", "     - converse: mic play failed: %v", err)
		return
	}
	b.statsMu.Lock()
	b.stats.SpokenChunks++
	b.statsMu.Unlock()
}

// drainSpeakCh removes all queued speak chunks without blocking; returns how many.
func (b *converseBridge) drainSpeakCh() int {
	n := 0
	for {
		select {
		case <-b.speakCh:
			n++
		default:
			return n
		}
	}
}

func (b *converseBridge) bumpGated() {
	b.statsMu.Lock()
	b.stats.GatedFrames++
	b.statsMu.Unlock()
}

// snapshot returns a race-free copy of the current stats (used by tests to poll
// side effects while the loops run).
func (b *converseBridge) snapshot() converseStats {
	b.statsMu.Lock()
	defer b.statsMu.Unlock()
	return b.stats
}

// --- pure helpers (unit-tested independent of goroutines) --------------------

// pcmFrameBytes is the byte length of one s16le frame of `dur` at rate/channels.
func pcmFrameBytes(rate, channels int, dur time.Duration) int {
	samples := int(float64(rate) * dur.Seconds())
	if samples < 1 {
		samples = 1
	}
	n := samples * channels * bytesPerSample
	if n < bytesPerSample {
		n = bytesPerSample
	}
	return n
}

// resamplePCM16 linearly resamples interleaved s16le PCM from inRate to outRate.
// A no-op when the rates match. Simple and allocation-light; good enough for a
// speech bridge (the capture endpoint normally serves the engine rate, so this is
// a safety path, not the hot path).
func resamplePCM16(in []byte, inRate, outRate, channels int) []byte {
	if inRate == outRate || inRate <= 0 || outRate <= 0 || channels <= 0 {
		return in
	}
	inFrames := len(in) / (channels * bytesPerSample)
	if inFrames < 2 {
		return in
	}
	outFrames := int(float64(inFrames) * float64(outRate) / float64(inRate))
	if outFrames < 1 {
		outFrames = 1
	}
	out := make([]byte, outFrames*channels*bytesPerSample)
	sample := func(frame, ch int) int16 {
		off := (frame*channels + ch) * bytesPerSample
		return int16(uint16(in[off]) | uint16(in[off+1])<<8)
	}
	for of := 0; of < outFrames; of++ {
		// Position in the input timeline.
		pos := float64(of) * float64(inRate) / float64(outRate)
		i0 := int(pos)
		frac := pos - float64(i0)
		i1 := i0 + 1
		if i1 >= inFrames {
			i1 = inFrames - 1
		}
		for ch := 0; ch < channels; ch++ {
			s0 := float64(sample(i0, ch))
			s1 := float64(sample(i1, ch))
			v := int16(s0 + (s1-s0)*frac)
			outOff := (of*channels + ch) * bytesPerSample
			out[outOff] = byte(uint16(v))
			out[outOff+1] = byte(uint16(v) >> 8)
		}
	}
	return out
}

// readFullFrame reads exactly len(buf) bytes unless the stream ends first. It
// returns the number of bytes read and any terminal error (io.EOF/ctx-close). A
// short final read still returns its bytes so a partial trailing frame is sent.
func readFullFrame(r io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// resetTimer safely resets t to d (draining a fired channel first).
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
