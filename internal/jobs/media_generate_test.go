package jobs

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestMediaGenerate_MissingPrompt(t *testing.T) {
	h := NewMediaGenerateHandler()
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "m1",
		Type:    "MEDIA_GENERATE",
		Payload: map[string]string{"task": "text-to-image"},
	})
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
}

func TestMediaGenerate_UnknownTask(t *testing.T) {
	h := NewMediaGenerateHandler()
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m2",
		Type: "MEDIA_GENERATE",
		Payload: map[string]string{
			"task":   "text-to-audio",
			"prompt": "a cat",
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
}

func TestMediaGenerate_EmptyTask(t *testing.T) {
	h := NewMediaGenerateHandler()
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "m3",
		Type:    "MEDIA_GENERATE",
		Payload: map[string]string{"prompt": "a cat"},
	})
	if err == nil {
		t.Fatal("expected error for missing task")
	}
}

// stubHealth writes a diffusers-service-shaped /health response.
func stubHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok","model":"stabilityai/sdxl-turbo","model_loaded":false,"device":"cuda","video_model":"Wan-AI/Wan2.1-T2V-1.3B-Diffusers","video_model_loaded":false,"video_device":"cuda"}`))
}

// TestMediaGenerate_ImageSuccess drives the handler against a stub diffusers
// sidecar for the text-to-image task. It must POST to /generate, and the
// outbound job envelope must satisfy the STRICT platform contract: encoding
// == "base64", content == the sidecar's image_base64 verbatim, format ==
// "png", model from the sidecar response, and a receipt object.
func TestMediaGenerate_ImageSuccess(t *testing.T) {
	imageBytes := []byte("fake-png-bytes")
	imageB64 := base64.StdEncoding.EncodeToString(imageBytes)

	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			stubHealth(w)
			return
		}
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":        "stabilityai/sdxl-turbo",
			"device":       "cuda",
			"width":        512,
			"height":       512,
			"seed":         nil,
			"image_base64": imageB64,
			"content_type": "image/png",
		})
	}))
	defer srv.Close()

	h := NewMediaGenerateHandler()
	h.ServiceURL = srv.URL

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m4",
		Type: "MEDIA_GENERATE",
		Payload: map[string]string{
			"task":                "text-to-image",
			"prompt":              "a cat astronaut",
			"num_inference_steps": "4",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/generate" {
		t.Errorf("hit path %q, want /generate", gotPath)
	}
	if gotBody["prompt"] != "a cat astronaut" {
		t.Errorf("forwarded prompt = %v", gotBody["prompt"])
	}
	if gotBody["num_inference_steps"] != float64(4) {
		t.Errorf("forwarded num_inference_steps = %v, want 4", gotBody["num_inference_steps"])
	}
	if _, present := gotBody["model"]; present {
		t.Errorf("model key must be OMITTED from sidecar request when payload model is empty, got %v", gotBody["model"])
	}
	if _, present := gotBody["width"]; present {
		t.Errorf("width key must be OMITTED when payload width is empty, got %v", gotBody["width"])
	}
	if _, present := gotBody["seed"]; present {
		t.Errorf("seed key must be OMITTED when payload seed is empty, got %v", gotBody["seed"])
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["encoding"] != "base64" {
		t.Errorf("encoding = %v, want base64", res["encoding"])
	}
	content, _ := res["content"].(string)
	if content == "" {
		t.Fatal("content must be non-empty")
	}
	if content != imageB64 {
		t.Errorf("content = %v, want the sidecar's image_base64 verbatim (%v)", content, imageB64)
	}
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		t.Fatalf("content is not valid base64: %v", err)
	}
	if string(decoded) != string(imageBytes) {
		t.Errorf("decoded content = %q, want %q", decoded, imageBytes)
	}
	if res["format"] != "png" {
		t.Errorf("format = %v, want png", res["format"])
	}
	if res["model"] != "stabilityai/sdxl-turbo" {
		t.Errorf("model = %v, want stabilityai/sdxl-turbo", res["model"])
	}
	receipt, ok := res["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt missing or wrong type: %v", res["receipt"])
	}
	if receipt["task"] != "text-to-image" {
		t.Errorf("receipt task = %v", receipt["task"])
	}
	if receipt["width"] != float64(512) {
		t.Errorf("receipt width = %v, want 512", receipt["width"])
	}
	if receipt["num_inference_steps"] != float64(4) {
		t.Errorf("receipt num_inference_steps = %v, want 4", receipt["num_inference_steps"])
	}
}

// TestMediaGenerate_VideoSuccess drives the handler against a stub diffusers
// sidecar for the text-to-video task: /generate/video, format=mp4.
func TestMediaGenerate_VideoSuccess(t *testing.T) {
	videoBytes := []byte("fake-mp4-bytes")
	videoB64 := base64.StdEncoding.EncodeToString(videoBytes)

	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			stubHealth(w)
			return
		}
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":        "Wan-AI/Wan2.1-T2V-1.3B-Diffusers",
			"device":       "cuda",
			"width":        832,
			"height":       480,
			"num_frames":   81,
			"fps":          16,
			"seed":         42,
			"video_base64": videoB64,
			"content_type": "video/mp4",
		})
	}))
	defer srv.Close()

	h := NewMediaGenerateHandler()
	h.ServiceURL = srv.URL

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m5",
		Type: "MEDIA_GENERATE",
		Payload: map[string]string{
			"task":       "text-to-video",
			"prompt":     "a dog running on a beach",
			"num_frames": "81",
			"fps":        "16",
			"seed":       "42",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/generate/video" {
		t.Errorf("hit path %q, want /generate/video", gotPath)
	}
	if gotBody["num_frames"] != float64(81) {
		t.Errorf("forwarded num_frames = %v, want 81", gotBody["num_frames"])
	}
	if gotBody["fps"] != float64(16) {
		t.Errorf("forwarded fps = %v, want 16", gotBody["fps"])
	}
	if gotBody["seed"] != float64(42) {
		t.Errorf("forwarded seed = %v, want 42", gotBody["seed"])
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["encoding"] != "base64" {
		t.Errorf("encoding = %v, want base64", res["encoding"])
	}
	content, _ := res["content"].(string)
	if content != videoB64 {
		t.Errorf("content = %v, want the sidecar's video_base64 verbatim", content)
	}
	if res["format"] != "mp4" {
		t.Errorf("format = %v, want mp4", res["format"])
	}
	if res["model"] != "Wan-AI/Wan2.1-T2V-1.3B-Diffusers" {
		t.Errorf("model = %v", res["model"])
	}
	receipt, ok := res["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt missing or wrong type: %v", res["receipt"])
	}
	if receipt["num_frames"] != float64(81) {
		t.Errorf("receipt num_frames = %v, want 81", receipt["num_frames"])
	}
	if receipt["fps"] != float64(16) {
		t.Errorf("receipt fps = %v, want 16", receipt["fps"])
	}
	if receipt["seed"] != float64(42) {
		t.Errorf("receipt seed = %v, want 42", receipt["seed"])
	}
}

// TestMediaGenerate_ModelForwardedWhenSpecified verifies a non-empty payload
// `model` IS forwarded to the sidecar under the "model" key.
func TestMediaGenerate_ModelForwardedWhenSpecified(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			stubHealth(w)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":        "custom/model",
			"width":        512,
			"height":       512,
			"image_base64": base64.StdEncoding.EncodeToString([]byte("x")),
			"content_type": "image/png",
		})
	}))
	defer srv.Close()

	h := NewMediaGenerateHandler()
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m6",
		Type: "MEDIA_GENERATE",
		Payload: map[string]string{
			"task":   "text-to-image",
			"prompt": "a cat",
			"model":  "custom/model",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["model"] != "custom/model" {
		t.Errorf("forwarded model = %v, want custom/model", gotBody["model"])
	}
}

// TestMediaGenerate_InvalidNumericStringsOmitted verifies garbage numeric
// payload strings are omitted rather than forwarded (e.g. as 0).
func TestMediaGenerate_InvalidNumericStringsOmitted(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			stubHealth(w)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":        "m",
			"width":        512,
			"height":       512,
			"image_base64": base64.StdEncoding.EncodeToString([]byte("x")),
			"content_type": "image/png",
		})
	}))
	defer srv.Close()

	h := NewMediaGenerateHandler()
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m7",
		Type: "MEDIA_GENERATE",
		Payload: map[string]string{
			"task":                "text-to-image",
			"prompt":              "a cat",
			"width":               "not-a-number",
			"num_inference_steps": "",
			"seed":                "also-garbage",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := gotBody["width"]; present {
		t.Errorf("invalid width must be OMITTED, not forwarded, got %v", gotBody["width"])
	}
	if _, present := gotBody["num_inference_steps"]; present {
		t.Errorf("empty num_inference_steps must be OMITTED, got %v", gotBody["num_inference_steps"])
	}
	if _, present := gotBody["seed"]; present {
		t.Errorf("invalid seed must be OMITTED, got %v", gotBody["seed"])
	}
}

func TestMediaGenerate_ServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			stubHealth(w)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"generation failed: CUDA OOM"}`))
	}))
	defer srv.Close()

	h := NewMediaGenerateHandler()
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "m8",
		Type: "MEDIA_GENERATE",
		Payload: map[string]string{
			"task":   "text-to-image",
			"prompt": "a cat",
		},
	})
	if err == nil {
		t.Fatal("expected error for non-200 service response")
	}
}

// TestMediaGenerate_WaitForReady_UnreachableFailsFast mirrors the synthesize
// handler's equivalent: an unreachable sidecar must fail fast, not hang near
// the full ready budget.
func TestMediaGenerate_WaitForReady_UnreachableFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := NewMediaGenerateHandler()
	h.ServiceURL = "http://" + addr

	start := time.Now()
	err = h.waitForReady()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for an unreachable sidecar")
	}
	if elapsed > mediaGenerateUnreachableTimeout+10*time.Second {
		t.Fatalf("waitForReady took %v, want close to the %v fast-fail budget (not the %v patient budget)", elapsed, mediaGenerateUnreachableTimeout, mediaGenerateReadyTimeout)
	}
}

// TestMediaGenerate_WaitForReady_DoesNotGateOnModelLoaded pins the deliberate
// divergence from SynthesizeSpeechHandler: the diffusers sidecar's /health
// never forces (or waits on) a model load, so waitForReady must return as
// soon as the sidecar answers 200 -- even with model_loaded:false -- rather
// than polling for a model_loaded flag that would never flip without an
// actual /generate call.
func TestMediaGenerate_WaitForReady_DoesNotGateOnModelLoaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","model_loaded":false,"video_model_loaded":false}`))
	}))
	defer srv.Close()

	h := NewMediaGenerateHandler()
	h.ServiceURL = srv.URL

	start := time.Now()
	if err := h.waitForReady(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForReady took %v, want near-immediate return on a reachable 200 regardless of model_loaded", elapsed)
	}
}
