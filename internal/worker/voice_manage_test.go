package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// VOICE_MANAGE must fail closed on the shared org pool. The sharedPoolQueue
// const (a queue with no ":node:" segment) and perNodeQueue are declared in
// module_set_test.go / agent_update_test.go in this same test package.

// fakeVoiceEngine stands in for the voice-clone module's /v1/voices HTTP API.
// It records what the handler sent (method, path, parsed multipart parts, and
// a request count) and returns a configurable status + body so a test can pin
// verbatim passthrough.
type fakeVoiceEngine struct {
	mu          sync.Mutex
	requests    int
	method      string
	path        string
	contentType string

	consentPresent bool
	consentValue   string
	name           string
	transcript     string
	engines        string
	audio          []byte

	status int
	body   string
	respCT string
}

func (f *fakeVoiceEngine) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		f.method = r.Method
		// EscapedPath() preserves the on-the-wire encoding, so a test can prove
		// url.PathEscape kept a "../x" voice_id as ONE segment (..%2Fx) rather
		// than letting it traverse; r.URL.Path would already be decoded.
		f.path = r.URL.EscapedPath()
		f.contentType = r.Header.Get("Content-Type")

		if strings.HasPrefix(f.contentType, "multipart/form-data") {
			if err := r.ParseMultipartForm(32 << 20); err == nil && r.MultipartForm != nil {
				if v, ok := r.MultipartForm.Value["consent"]; ok && len(v) > 0 {
					f.consentPresent = true
					f.consentValue = v[0]
				}
				if v, ok := r.MultipartForm.Value["name"]; ok && len(v) > 0 {
					f.name = v[0]
				}
				if v, ok := r.MultipartForm.Value["transcript"]; ok && len(v) > 0 {
					f.transcript = v[0]
				}
				if v, ok := r.MultipartForm.Value["engines"]; ok && len(v) > 0 {
					f.engines = v[0]
				}
				if fhs, ok := r.MultipartForm.File["audio"]; ok && len(fhs) > 0 {
					fp, openErr := fhs[0].Open()
					if openErr == nil {
						f.audio, _ = io.ReadAll(fp)
						fp.Close()
					}
				}
			}
		}

		ct := f.respCT
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	}
}

func (f *fakeVoiceEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// startFakeEngine mounts the fake as a real loopback server and returns it,
// registering teardown.
func startFakeEngine(t *testing.T, engine *fakeVoiceEngine) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(engine.handler())
	t.Cleanup(srv.Close)
	return srv
}

func voiceJob(queue string, payload map[string]any) *Job {
	return &Job{ID: "vm-test", Type: JobTypeVoiceManage, SourceQueue: queue, Payload: payload}
}

func mustExecute(t *testing.T, h *VoiceManageHandler, job *Job) *JobResult {
	t.Helper()
	res, err := h.Execute(context.Background(), job, &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("Execute returned a Go error (handlers report via JobResult): %v", err)
	}
	if res == nil {
		t.Fatal("Execute returned a nil result")
	}
	return res
}

func TestVoiceManage_CanHandle(t *testing.T) {
	h := NewVoiceManageHandler(VoiceManageConfig{})
	if !h.CanHandle(JobTypeVoiceManage) {
		t.Error("must handle VOICE_MANAGE")
	}
	if h.CanHandle(JobTypeExposeSet) || h.CanHandle("OTHER") {
		t.Error("must not handle unrelated job types")
	}
}

// TestVoiceManage_RefusedOffPerNodeStream is the fail-closed teeth: a job on
// the shared org pool is refused BEFORE any HTTP call to the module.
func TestVoiceManage_RefusedOffPerNodeStream(t *testing.T) {
	engine := &fakeVoiceEngine{status: 200, body: `{"voices":[]}`}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	res := mustExecute(t, h, voiceJob(sharedPoolQueue, map[string]any{"action": "list"}))
	if res.Status != JobStatusFailure {
		t.Fatalf("shared-pool job must fail closed, got %v", res.Status)
	}
	if res.Error == nil || !strings.Contains(res.Error.Error(), "per-node stream") {
		t.Fatalf("error must explain the per-node gate, got %v", res.Error)
	}
	if engine.count() != 0 {
		t.Fatalf("a refused job must never reach the module (requests=%d)", engine.count())
	}
}

func TestVoiceManage_ListProxiesGet(t *testing.T) {
	engine := &fakeVoiceEngine{status: 200, body: `{"voices":[{"voice_id":"v1"}]}`}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	res := mustExecute(t, h, voiceJob(perNodeQueue, map[string]any{"action": "list"}))
	if res.Status != JobStatusSuccess {
		t.Fatalf("list should succeed as a proxy, got %v (%v)", res.Status, res.Error)
	}
	if engine.method != http.MethodGet || engine.path != "/v1/voices" {
		t.Fatalf("list must GET /v1/voices, saw %s %s", engine.method, engine.path)
	}
	if got := res.Output["body"]; got != engine.body {
		t.Fatalf("body not relayed verbatim: got %q want %q", got, engine.body)
	}
	if res.Output["status_code"] != 200 || res.Output["ok"] != true {
		t.Fatalf("unexpected status envelope: %+v", res.Output)
	}
}

func TestVoiceManage_DeleteProxiesDeleteAndEscapes(t *testing.T) {
	cases := []struct {
		name     string
		voiceID  string
		wantPath string
	}{
		{"plain", "abc123", "/v1/voices/abc123"},
		// A traversal attempt is path-escaped into a single segment the module
		// resolves and rejects (404) — never traversal on our side.
		{"traversal escaped", "../secret", "/v1/voices/..%2Fsecret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeVoiceEngine{status: 204, body: ""}
			srv := startFakeEngine(t, engine)
			h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

			res := mustExecute(t, h, voiceJob(perNodeQueue, map[string]any{"action": "delete", "voice_id": tc.voiceID}))
			if res.Status != JobStatusSuccess {
				t.Fatalf("delete should succeed as a proxy, got %v (%v)", res.Status, res.Error)
			}
			if engine.method != http.MethodDelete {
				t.Fatalf("delete must use DELETE, saw %s", engine.method)
			}
			if engine.path != tc.wantPath {
				t.Fatalf("delete path = %q, want %q", engine.path, tc.wantPath)
			}
		})
	}
}

func TestVoiceManage_DeleteRequiresVoiceID(t *testing.T) {
	engine := &fakeVoiceEngine{status: 204}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	res := mustExecute(t, h, voiceJob(perNodeQueue, map[string]any{"action": "delete"}))
	if res.Status != JobStatusFailure {
		t.Fatalf("delete without voice_id must fail, got %v", res.Status)
	}
	if engine.count() != 0 {
		t.Fatalf("a bad payload must never reach the module (requests=%d)", engine.count())
	}
}

func TestVoiceManage_UnknownActionFailsWithoutRequest(t *testing.T) {
	engine := &fakeVoiceEngine{status: 200}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	for _, action := range []string{"", "frobnicate"} {
		res := mustExecute(t, h, voiceJob(perNodeQueue, map[string]any{"action": action}))
		if res.Status != JobStatusFailure {
			t.Fatalf("action %q must fail, got %v", action, res.Status)
		}
	}
	if engine.count() != 0 {
		t.Fatalf("an unknown action must never reach the module (requests=%d)", engine.count())
	}
}

func TestVoiceManage_EnrollForwardsConsentAndAudio(t *testing.T) {
	engine := &fakeVoiceEngine{status: 201, body: `{"voice_id":"v9","consent":{}}`}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	consent := map[string]any{
		"speaker_name": "Ada",
		"attested_by":  "Ada Lovelace",
		"statement":    "I consent to having my voice cloned.",
		"attested_at":  "2026-10-09T00:00:00Z",
	}
	audioBytes := []byte("RIFF....fake-wav-bytes")
	payload := map[string]any{
		"action":       "enroll",
		"consent":      consent,
		"audio_base64": base64.StdEncoding.EncodeToString(audioBytes),
		"name":         "Ada Voice",
		"transcript":   "hello world",
		"engines":      []any{"chatterbox", "xtts"},
	}

	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusSuccess {
		t.Fatalf("enroll should succeed as a proxy, got %v (%v)", res.Status, res.Error)
	}
	if engine.method != http.MethodPost || engine.path != "/v1/voices" {
		t.Fatalf("enroll must POST /v1/voices, saw %s %s", engine.method, engine.path)
	}
	if !strings.HasPrefix(engine.contentType, "multipart/form-data") {
		t.Fatalf("enroll must be multipart/form-data, saw %q", engine.contentType)
	}
	// Consent is forwarded VERBATIM (object -> JSON string), never re-shaped.
	wantConsent, _ := json.Marshal(consent)
	if !engine.consentPresent || engine.consentValue != string(wantConsent) {
		t.Fatalf("consent not forwarded verbatim: present=%v value=%q want=%q", engine.consentPresent, engine.consentValue, wantConsent)
	}
	if string(engine.audio) != string(audioBytes) {
		t.Fatalf("audio not forwarded intact: got %q want %q", engine.audio, audioBytes)
	}
	if engine.name != "Ada Voice" || engine.transcript != "hello world" || engine.engines != "chatterbox,xtts" {
		t.Fatalf("optional fields wrong: name=%q transcript=%q engines=%q", engine.name, engine.transcript, engine.engines)
	}
}

// TestVoiceManage_MissingConsentForwardedSoEngineRefuses is the core contract:
// the handler must NOT short-circuit a missing consent. It forwards the request
// WITHOUT a consent part and relays the module's own 422 body byte-identical.
func TestVoiceManage_MissingConsentForwardedSoEngineRefuses(t *testing.T) {
	const engine422 = `{"detail":"consent is required: an object with speaker_name, attested_by, statement and attested_at (ISO 8601) recording that the speaker agreed to be cloned"}`
	engine := &fakeVoiceEngine{status: 422, body: engine422}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	payload := map[string]any{
		"action":       "enroll",
		"audio_base64": base64.StdEncoding.EncodeToString([]byte("clip")),
		// no consent
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusSuccess {
		t.Fatalf("a module refusal is a successful proxy, got %v (%v)", res.Status, res.Error)
	}
	if engine.count() != 1 || engine.consentPresent {
		t.Fatalf("request must be forwarded with NO consent part (requests=%d consentPresent=%v)", engine.count(), engine.consentPresent)
	}
	if res.Output["status_code"] != 422 {
		t.Fatalf("status_code = %v, want 422", res.Output["status_code"])
	}
	if res.Output["ok"] != false {
		t.Fatalf("ok = %v, want false", res.Output["ok"])
	}
	if res.Output["body"] != engine422 {
		t.Fatalf("422 body not relayed verbatim:\n got %q\nwant %q", res.Output["body"], engine422)
	}
}

// TestVoiceManage_LicenseGate403BodyVerbatim pins that a 403 license refusal is
// relayed unchanged (not rewritten or swallowed).
func TestVoiceManage_LicenseGate403BodyVerbatim(t *testing.T) {
	const engine403 = `{"detail":"voice cloning is disabled: the engine weights are non-commercial and VOICE_CLONE_ACCEPT_LICENSE is unset"}`
	engine := &fakeVoiceEngine{status: 403, body: engine403}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	payload := map[string]any{
		"action":       "enroll",
		"consent":      map[string]any{"speaker_name": "A", "attested_by": "B", "statement": "ok", "attested_at": "2026-10-09T00:00:00Z"},
		"audio_base64": base64.StdEncoding.EncodeToString([]byte("clip")),
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusSuccess {
		t.Fatalf("a 403 refusal is a successful proxy, got %v (%v)", res.Status, res.Error)
	}
	if res.Output["status_code"] != 403 || res.Output["ok"] != false {
		t.Fatalf("unexpected envelope: %+v", res.Output)
	}
	if res.Output["body"] != engine403 {
		t.Fatalf("403 body not relayed verbatim:\n got %q\nwant %q", res.Output["body"], engine403)
	}
}

func TestVoiceManage_EnrollOverCapRejectedWithoutRequest(t *testing.T) {
	engine := &fakeVoiceEngine{status: 201}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL, MaxUploadBytes: 8})

	payload := map[string]any{
		"action":       "enroll",
		"consent":      map[string]any{"speaker_name": "A", "attested_by": "B", "statement": "ok", "attested_at": "2026-10-09T00:00:00Z"},
		"audio_base64": base64.StdEncoding.EncodeToString([]byte("way more than eight bytes")),
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusFailure {
		t.Fatalf("over-cap clip must fail, got %v", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "cap") {
		t.Fatalf("error should mention the cap, got %v", res.Error)
	}
	if engine.count() != 0 {
		t.Fatalf("an over-cap clip must never reach the module (requests=%d)", engine.count())
	}
}

func TestVoiceManage_EnrollRequiresAudio(t *testing.T) {
	engine := &fakeVoiceEngine{status: 201}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL})

	payload := map[string]any{
		"action":  "enroll",
		"consent": map[string]any{"speaker_name": "A", "attested_by": "B", "statement": "ok", "attested_at": "2026-10-09T00:00:00Z"},
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusFailure {
		t.Fatalf("enroll without audio must fail, got %v", res.Status)
	}
	if engine.count() != 0 {
		t.Fatalf("missing audio must never reach the module (requests=%d)", engine.count())
	}
}

func TestVoiceManage_EnrollAudioPathReadsWorkspaceClip(t *testing.T) {
	workspace := t.TempDir()
	clip := []byte("workspace-delivered-wav-bytes")
	if err := os.WriteFile(filepath.Join(workspace, "ref.wav"), clip, 0o644); err != nil {
		t.Fatal(err)
	}
	engine := &fakeVoiceEngine{status: 201, body: `{"voice_id":"vw"}`}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL, WorkspaceDir: workspace})

	payload := map[string]any{
		"action":     "enroll",
		"consent":    map[string]any{"speaker_name": "A", "attested_by": "B", "statement": "ok", "attested_at": "2026-10-09T00:00:00Z"},
		"audio_path": "ref.wav",
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusSuccess {
		t.Fatalf("workspace-clip enroll should succeed, got %v (%v)", res.Status, res.Error)
	}
	if string(engine.audio) != string(clip) {
		t.Fatalf("workspace clip not forwarded: got %q want %q", engine.audio, clip)
	}
}

func TestVoiceManage_EnrollAudioPathTraversalRefused(t *testing.T) {
	workspace := t.TempDir()
	engine := &fakeVoiceEngine{status: 201}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL, WorkspaceDir: workspace})

	payload := map[string]any{
		"action":     "enroll",
		"consent":    map[string]any{"speaker_name": "A", "attested_by": "B", "statement": "ok", "attested_at": "2026-10-09T00:00:00Z"},
		"audio_path": "../../etc/passwd",
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusFailure {
		t.Fatalf("a traversal audio_path must be refused, got %v", res.Status)
	}
	if engine.count() != 0 {
		t.Fatalf("a refused audio_path must never reach the module (requests=%d)", engine.count())
	}
}

func TestVoiceManage_EnrollAudioPathWithoutWorkspaceRefused(t *testing.T) {
	engine := &fakeVoiceEngine{status: 201}
	srv := startFakeEngine(t, engine)
	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: srv.URL}) // no WorkspaceDir

	payload := map[string]any{
		"action":     "enroll",
		"consent":    map[string]any{"speaker_name": "A", "attested_by": "B", "statement": "ok", "attested_at": "2026-10-09T00:00:00Z"},
		"audio_path": "ref.wav",
	}
	res := mustExecute(t, h, voiceJob(perNodeQueue, payload))
	if res.Status != JobStatusFailure {
		t.Fatalf("audio_path without a workspace must fail, got %v", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "workspace") {
		t.Fatalf("error should mention the missing workspace, got %v", res.Error)
	}
	if engine.count() != 0 {
		t.Fatalf("requests=%d, want 0", engine.count())
	}
}

// TestVoiceManage_UnreachableModuleClearFailure pins the "module not installed"
// case: a transport error is a terminal failure with a clear, actionable
// message naming the voice-clone module — not a crash, not a silent retry loop.
func TestVoiceManage_UnreachableModuleClearFailure(t *testing.T) {
	engine := &fakeVoiceEngine{status: 200, body: "{}"}
	srv := startFakeEngine(t, engine)
	closedURL := srv.URL
	srv.Close() // nothing is listening now

	h := NewVoiceManageHandler(VoiceManageConfig{BaseURL: closedURL})
	res := mustExecute(t, h, voiceJob(perNodeQueue, map[string]any{"action": "list"}))
	if res.Status != JobStatusFailure {
		t.Fatalf("unreachable module must fail, got %v", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "voice-clone") {
		t.Fatalf("error must name the voice-clone module, got %v", res.Error)
	}
}

// TestVoiceManage_ZeroConfigConstructs proves the handler is constructable with
// a zero config (no workspace, no custom URL), which is what lets cmd/nodejobs.go
// register it unconditionally (like RESOURCE_SNAPSHOT) without a gate. The actual
// registration wiring is covered by the cmd package build + tests.
func TestVoiceManage_ZeroConfigConstructs(t *testing.T) {
	h := NewVoiceManageHandler(VoiceManageConfig{})
	if h == nil || !h.CanHandle(JobTypeVoiceManage) {
		t.Fatal("VOICE_MANAGE handler must be constructable with a zero config")
	}
}
