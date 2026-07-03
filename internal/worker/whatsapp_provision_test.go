package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
)

func whatsappJob(sourceQueue string, payload map[string]any) *Job {
	if payload == nil {
		payload = map[string]any{}
	}
	return &Job{ID: "wa-1", Type: JobTypeWhatsAppProvision, SourceQueue: sourceQueue, Payload: payload}
}

// decodeOutput unwraps the {"output": "<json>"} wire shape and parses the inner
// contract document, mirroring how the aceteam backend consumes the result.
func decodeOutput(t *testing.T, res *JobResult) map[string]any {
	t.Helper()
	raw, ok := res.Output["output"].(string)
	if !ok {
		t.Fatalf("result output missing 'output' string; got %#v", res.Output)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, raw)
	}
	return doc
}

func TestWhatsAppProvisionHappyPath(t *testing.T) {
	var gotReq whatsapp.ProvisionRequest
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			gotReq = req
			return &whatsapp.ProvisionResult{
				APIURL: "http://100.64.0.9:8080",
				APIKey: "wab_key",
				QR:     "2@payload",
				Tenant: "default",
			}, nil
		},
	})

	res, err := h.Execute(context.Background(), whatsappJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success", res.Status)
	}
	if gotReq.Tenant != "default" {
		t.Errorf("tenant defaulted to %q, want default", gotReq.Tenant)
	}
	doc := decodeOutput(t, res)
	if doc["api_url"] != "http://100.64.0.9:8080" {
		t.Errorf("api_url = %v, want mesh url", doc["api_url"])
	}
	if doc["api_key"] != "wab_key" {
		t.Errorf("api_key = %v", doc["api_key"])
	}
	if doc["tenant"] != "default" {
		t.Errorf("tenant = %v, want default", doc["tenant"])
	}
	if doc["status"] != "provisioned" {
		t.Errorf("status = %v, want provisioned", doc["status"])
	}
	qr, _ := doc["qr"].(string)
	if !strings.HasPrefix(qr, "data:image/png;base64,") {
		t.Errorf("qr = %q, want a data-url PNG", qr)
	}
}

func TestWhatsAppProvisionParsesPayload(t *testing.T) {
	var gotReq whatsapp.ProvisionRequest
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			gotReq = req
			return &whatsapp.ProvisionResult{APIURL: "u", APIKey: "k", Tenant: req.Tenant, AlreadyLinked: true}, nil
		},
	})
	// "port" arrives as a JSON number (float64) and must coerce to the int
	// override. When absent it stays 0 so Provision auto-selects (issue #438).
	payload := map[string]any{"tenant": "sales", "proxy": "socks5://p", "public_url": "https://pub", "port": float64(8091)}
	if _, err := h.Execute(context.Background(), whatsappJob(perNodeQueue, payload), &NoOpStreamWriter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq.Tenant != "sales" || gotReq.Proxy != "socks5://p" || gotReq.PublicURL != "https://pub" {
		t.Errorf("payload not parsed into request: %+v", gotReq)
	}
	if gotReq.Port != 8091 {
		t.Errorf("port not parsed into request: got %d, want 8091", gotReq.Port)
	}
}

func TestWhatsAppProvisionDefaultsPortToAuto(t *testing.T) {
	var gotReq whatsapp.ProvisionRequest
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			gotReq = req
			return &whatsapp.ProvisionResult{APIURL: "u", APIKey: "k", Tenant: "default"}, nil
		},
	})
	// No "port" in the payload -> Port stays 0 so Provision auto-selects a free
	// host port rather than colliding on 8080.
	if _, err := h.Execute(context.Background(), whatsappJob(perNodeQueue, nil), &NoOpStreamWriter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq.Port != 0 {
		t.Errorf("Port = %d, want 0 (auto-select) when payload omits port", gotReq.Port)
	}
}

func TestWhatsAppProvisionAlreadyLinked(t *testing.T) {
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			return &whatsapp.ProvisionResult{
				APIURL:        "http://100.64.0.9:8080",
				APIKey:        "wab_key",
				QR:            "ignored-because-linked",
				Tenant:        "default",
				AlreadyLinked: true,
			}, nil
		},
	})
	res, err := h.Execute(context.Background(), whatsappJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	doc := decodeOutput(t, res)
	if doc["status"] != "already_linked" {
		t.Errorf("status = %v, want already_linked", doc["status"])
	}
	if doc["qr"] != "" {
		t.Errorf("qr = %v, want empty for already-linked", doc["qr"])
	}
	if doc["api_url"] == "" || doc["api_key"] == "" {
		t.Error("already-linked result must still carry api_url + api_key")
	}
}

func TestWhatsAppProvisionPerNodeGate(t *testing.T) {
	called := false
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			called = true
			return &whatsapp.ProvisionResult{}, nil
		},
	})
	// Shared org pool (no ":node:" segment) must be refused before provisioning.
	res, err := h.Execute(context.Background(), whatsappJob("jobs:v1:shell:org_test", nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure on the shared pool", res.Status)
	}
	if called {
		t.Error("Provision must not run for a job off the per-node stream")
	}
	if !strings.Contains(res.Output["error"].(string), "per-node") {
		t.Errorf("error = %v, want it to explain the per-node gate", res.Output["error"])
	}
}

func TestWhatsAppProvisionDockerOrCredsMissing(t *testing.T) {
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			// Simulates the deploy edge failing fast because Docker is down or the
			// private module repo can't be cloned for lack of credentials.
			return nil, errors.New("resolve WhatsApp bridge module (needs Docker + private-repo git credentials): git clone failed")
		},
	})
	res, err := h.Execute(context.Background(), whatsappJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure", res.Status)
	}
	msg, _ := res.Output["error"].(string)
	if !strings.Contains(msg, "Docker") && !strings.Contains(msg, "credentials") {
		t.Errorf("error = %q, want a clear Docker/creds message", msg)
	}
}

func TestWhatsAppProvisionMisconfigured(t *testing.T) {
	// Nil Provision must yield a clear failure, not a panic.
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{})
	res, err := h.Execute(context.Background(), whatsappJob(perNodeQueue, nil), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Errorf("status = %v, want failure when misconfigured", res.Status)
	}
}

func TestWhatsAppProvisionCanHandle(t *testing.T) {
	h := NewWhatsAppProvisionHandler(WhatsAppProvisionConfig{})
	if !h.CanHandle(JobTypeWhatsAppProvision) {
		t.Error("CanHandle(WHATSAPP_PROVISION) = false, want true")
	}
	if h.CanHandle(JobTypeAgentUpdate) {
		t.Error("CanHandle(AGENT_UPDATE) = true, want false")
	}
}
