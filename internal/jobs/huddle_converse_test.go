package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// --- mocks -------------------------------------------------------------------

// mockRealtimeConn is an injectable realtimeConn: Send records outbound frames,
// Recv yields queued inbound events (and blocks until closed so the bridge stays
// resident like a live engine).
type mockRealtimeConn struct {
	mu       sync.Mutex
	sent     [][]byte
	incoming chan []byte
	closed   chan struct{}
	once     sync.Once
}

func newMockConn() *mockRealtimeConn {
	return &mockRealtimeConn{incoming: make(chan []byte, 32), closed: make(chan struct{})}
}

func (m *mockRealtimeConn) Send(p []byte) error {
	m.mu.Lock()
	m.sent = append(m.sent, append([]byte(nil), p...))
	m.mu.Unlock()
	return nil
}

func (m *mockRealtimeConn) Recv() ([]byte, error) {
	select {
	case msg := <-m.incoming:
		return msg, nil
	case <-m.closed:
		return nil, io.EOF
	}
}

func (m *mockRealtimeConn) Close() error {
	m.once.Do(func() { close(m.closed) })
	return nil
}

func (m *mockRealtimeConn) sentTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, raw := range m.sent {
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &ev)
		out = append(out, ev.Type)
	}
	return out
}

func (m *mockRealtimeConn) countType(t string) int {
	n := 0
	for _, got := range m.sentTypes() {
		if got == t {
			n++
		}
	}
	return n
}

// mockConverseMedia records SpeakPCM calls and serves a caller-supplied capture
// stream. SpeakPCM can be made to block on speakGate to simulate a long clip.
type mockConverseMedia struct {
	capture   io.ReadCloser
	captureFn func(ctx context.Context) (io.ReadCloser, error)

	mu        sync.Mutex
	played    [][]byte
	speakGate chan struct{} // nil => don't block
}

func (m *mockConverseMedia) CaptureStream(ctx context.Context, rate, channels int) (io.ReadCloser, error) {
	if m.captureFn != nil {
		return m.captureFn(ctx)
	}
	return m.capture, nil
}

func (m *mockConverseMedia) SpeakPCM(pcm []byte, rate, channels int) error {
	if m.speakGate != nil {
		<-m.speakGate
	}
	m.mu.Lock()
	m.played = append(m.played, append([]byte(nil), pcm...))
	m.mu.Unlock()
	return nil
}

func (m *mockConverseMedia) playedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.played)
}

func (m *mockConverseMedia) playedBytes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.played {
		n += len(p)
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func audioDeltaMsg(pcm []byte) []byte {
	b, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{Type: evtAudioDelta, Delta: base64.StdEncoding.EncodeToString(pcm)})
	return b
}

func speechStartedMsg() []byte {
	b, _ := json.Marshal(struct {
		Type string `json:"type"`
	}{Type: evtSpeechStarted})
	return b
}

// fastConfig makes frames tiny and timers short so tests run in milliseconds.
func fastConfig() converseConfig {
	return converseConfig{
		EngineRate:        8000,
		EngineChannels:    1,
		CaptureRate:       8000,
		FrameDuration:     1 * time.Millisecond, // 8 samples -> 16 bytes/frame
		SpeakCoalesce:     5 * time.Millisecond,
		SpeakHangover:     5 * time.Millisecond,
		MicBusyBackoff:    1 * time.Millisecond,
		StatePollInterval: 5 * time.Millisecond,
	}
}

// --- tests -------------------------------------------------------------------

// TestConverseBridge_ForwardsCapturedAudio asserts captured room PCM is forwarded
// to the engine as input_audio_buffer.append frames, and that with server VAD the
// bridge never sends commit/response.create.
func TestConverseBridge_ForwardsCapturedAudio(t *testing.T) {
	cfg := fastConfig()
	pr, pw := io.Pipe()
	media := &mockConverseMedia{capture: pr}
	conn := newMockConn()
	b := newConverseBridge(conn, media, nil, nil, cfg)

	go b.Run(context.Background())

	frameBytes := pcmFrameBytes(cfg.CaptureRate, cfg.EngineChannels, cfg.FrameDuration)
	// Write 3 full frames of non-silent audio.
	payload := make([]byte, frameBytes*3)
	for i := range payload {
		payload[i] = byte(i%200 + 1)
	}
	if _, err := pw.Write(payload); err != nil {
		t.Fatalf("write capture: %v", err)
	}

	waitFor(t, "3 appends forwarded", func() bool { return conn.countType(msgInputAudioAppend) >= 3 })

	if got := conn.countType("input_audio_buffer.commit"); got != 0 {
		t.Errorf("sent %d commit frames; server VAD means we must never force a turn", got)
	}
	if got := conn.countType("response.create"); got != 0 {
		t.Errorf("sent %d response.create; must never force a turn under server VAD", got)
	}
	_ = pw.Close()
	conn.Close()
}

// TestConverseBridge_PlaysAudioDeltas asserts engine audio deltas are decoded and
// injected into the virtual mic via SpeakPCM.
func TestConverseBridge_PlaysAudioDeltas(t *testing.T) {
	cfg := fastConfig()
	pr, _ := io.Pipe() // capture blocks (no room audio) so only the SPEAK path runs
	media := &mockConverseMedia{capture: pr}
	conn := newMockConn()
	b := newConverseBridge(conn, media, nil, nil, cfg)

	go b.Run(context.Background())

	ttsPCM := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	conn.incoming <- audioDeltaMsg(ttsPCM)

	waitFor(t, "delta played into mic", func() bool { return media.playedCount() >= 1 })
	waitFor(t, "audio delta counted", func() bool { return b.snapshot().AudioDeltas >= 1 })

	if media.playedBytes() < len(ttsPCM) {
		t.Errorf("played %d bytes, want >= %d", media.playedBytes(), len(ttsPCM))
	}
	conn.Close()
}

// TestConverseBridge_StopsOnLeft asserts the bridge stops when the state check
// reports the call ended.
func TestConverseBridge_StopsOnLeft(t *testing.T) {
	cfg := fastConfig()
	pr, _ := io.Pipe()
	media := &mockConverseMedia{capture: pr}
	conn := newMockConn()

	ended := make(chan struct{})
	var once sync.Once
	state := func() (bool, string) {
		select {
		case <-ended:
			return true, "bot left the call"
		default:
			once.Do(func() { close(ended) }) // end on the first poll
			return false, ""
		}
	}
	b := newConverseBridge(conn, media, state, nil, cfg)

	done := make(chan converseStats, 1)
	go func() { s, _ := b.Run(context.Background()); done <- s }()

	select {
	case s := <-done:
		if s.StopReason != "bot left the call" {
			t.Errorf("StopReason = %q, want %q", s.StopReason, "bot left the call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not stop on left state")
	}
}

// TestConverseBridge_StopsOnContextCancel asserts a cancelled parent context ends
// the resident loop.
func TestConverseBridge_StopsOnContextCancel(t *testing.T) {
	cfg := fastConfig()
	pr, _ := io.Pipe()
	media := &mockConverseMedia{capture: pr}
	conn := newMockConn()
	b := newConverseBridge(conn, media, nil, nil, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan converseStats, 1)
	go func() { s, _ := b.Run(ctx); done <- s }()

	cancel()
	select {
	case s := <-done:
		if s.StopReason == "" {
			t.Error("expected a stop reason on context cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not stop on context cancel")
	}
}

// TestConverseBridge_GateDropsFramesWhileSpeaking asserts that with the (default)
// speaking gate on, captured frames are dropped while the bot is playing TTS, so
// the bot's own audio can't be fed back as user speech.
func TestConverseBridge_GateDropsFramesWhileSpeaking(t *testing.T) {
	cfg := fastConfig()
	pr, pw := io.Pipe()
	gate := make(chan struct{})
	media := &mockConverseMedia{capture: pr, speakGate: gate}
	conn := newMockConn()
	b := newConverseBridge(conn, media, nil, nil, cfg)

	go b.Run(context.Background())

	// Kick off a TTS delta; SpeakPCM blocks on `gate`, so `speaking` stays true.
	conn.incoming <- audioDeltaMsg([]byte{9, 9, 9, 9})
	waitFor(t, "bridge entered speaking state", func() bool { return b.speaking.Load() })

	// Now push room audio: it must be gated (dropped), not appended.
	frameBytes := pcmFrameBytes(cfg.CaptureRate, cfg.EngineChannels, cfg.FrameDuration)
	if _, err := pw.Write(make([]byte, frameBytes*3)); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	waitFor(t, "frames gated while speaking", func() bool { return b.snapshot().GatedFrames >= 3 })

	if got := conn.countType(msgInputAudioAppend); got != 0 {
		t.Errorf("appended %d frames while speaking; gate should have dropped them", got)
	}

	// Release playback; the gate reopens (hangover) and a clear is sent.
	close(gate)
	waitFor(t, "clear sent on gate release", func() bool { return conn.countType(msgInputAudioClear) >= 1 })
	conn.Close()
}

// TestConverseBridge_BargeInDropsQueuedTTS asserts a speech_started event drops
// TTS still queued to play (the human interrupted), counted as a barge-in.
func TestConverseBridge_BargeInDropsQueuedTTS(t *testing.T) {
	cfg := fastConfig()
	pr, _ := io.Pipe()
	gate := make(chan struct{})
	media := &mockConverseMedia{capture: pr, speakGate: gate}
	conn := newMockConn()
	b := newConverseBridge(conn, media, nil, nil, cfg)

	go b.Run(context.Background())

	// First delta enters the speaker and blocks on `gate`; subsequent deltas queue.
	conn.incoming <- audioDeltaMsg([]byte{1, 1})
	waitFor(t, "speaker holding first chunk", func() bool { return b.speaking.Load() })
	// Let the first chunk's coalesce window close and playback block on `gate`, so
	// the next deltas queue in speakCh rather than being folded into the first play.
	time.Sleep(10 * cfg.SpeakCoalesce)
	// Queue several more chunks behind the blocked speaker.
	for i := 0; i < 5; i++ {
		conn.incoming <- audioDeltaMsg([]byte{byte(i), byte(i)})
	}
	waitFor(t, "chunks queued", func() bool { return len(b.speakCh) > 0 })

	// Human starts talking -> drop the queued TTS.
	conn.incoming <- speechStartedMsg()
	waitFor(t, "barge-in recorded", func() bool { return b.snapshot().Barges >= 1 })
	waitFor(t, "speak queue drained", func() bool { return len(b.speakCh) == 0 })

	close(gate)
	conn.Close()
}

// TestResamplePCM16 checks the linear resampler: a no-op when rates match, and the
// right output frame count when up/down-sampling, preserving endpoints.
func TestResamplePCM16(t *testing.T) {
	// s16le, mono. 4 samples: 0, 100, 200, 300.
	in := pcm16([]int16{0, 100, 200, 300})

	if got := resamplePCM16(in, 8000, 8000, 1); len(got) != len(in) {
		t.Errorf("equal-rate resample changed length: got %d want %d", len(got), len(in))
	}

	// Upsample 8k -> 16k doubles frame count (approx).
	up := resamplePCM16(in, 8000, 16000, 1)
	upSamples := len(up) / 2
	if upSamples < 7 || upSamples > 9 {
		t.Errorf("upsample 8k->16k of 4 samples = %d samples, want ~8", upSamples)
	}
	// First sample preserved.
	if first := int16(uint16(up[0]) | uint16(up[1])<<8); first != 0 {
		t.Errorf("first upsampled sample = %d, want 0", first)
	}

	// Downsample 16k -> 8k halves frame count.
	down := resamplePCM16(pcm16([]int16{0, 50, 100, 150, 200, 250, 300, 350}), 16000, 8000, 1)
	if ds := len(down) / 2; ds < 3 || ds > 5 {
		t.Errorf("downsample 16k->8k of 8 samples = %d samples, want ~4", ds)
	}
}

func TestPCMFrameBytes(t *testing.T) {
	// 20ms @ 24kHz mono s16le = 480 samples * 2 = 960 bytes.
	if got := pcmFrameBytes(24000, 1, 20*time.Millisecond); got != 960 {
		t.Errorf("pcmFrameBytes(24000,1,20ms) = %d, want 960", got)
	}
	// Stereo doubles it.
	if got := pcmFrameBytes(24000, 2, 20*time.Millisecond); got != 1920 {
		t.Errorf("pcmFrameBytes(24000,2,20ms) = %d, want 1920", got)
	}
	// A sub-sample duration still yields at least one sample.
	if got := pcmFrameBytes(24000, 1, 0); got < bytesPerSample {
		t.Errorf("pcmFrameBytes with 0 dur = %d, want >= %d", got, bytesPerSample)
	}
}

func TestRealtimeWSURL(t *testing.T) {
	got, err := realtimeWSURL("https://aceteam.ai", "", "agent-9")
	if err != nil {
		t.Fatalf("realtimeWSURL: %v", err)
	}
	// https -> wss, agentId + server_vad injected.
	if !contains(got, "wss://aceteam.ai/api/realtime") {
		t.Errorf("url = %q, want wss endpoint", got)
	}
	if !contains(got, "agentId=agent-9") {
		t.Errorf("url = %q, missing agentId", got)
	}
	if !contains(got, "turnDetection=server_vad") {
		t.Errorf("url = %q, missing server_vad turn detection", got)
	}

	// Explicit override is honored and upgraded http->ws.
	got2, err := realtimeWSURL("https://aceteam.ai", "http://localhost:3000/api/realtime", "a")
	if err != nil {
		t.Fatalf("realtimeWSURL override: %v", err)
	}
	if !contains(got2, "ws://localhost:3000/api/realtime") {
		t.Errorf("override url = %q, want ws://localhost endpoint", got2)
	}
}

// pcm16 encodes int16 samples as little-endian bytes for the resampler tests.
func pcm16(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		out[i*2] = byte(uint16(s))
		out[i*2+1] = byte(uint16(s) >> 8)
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
