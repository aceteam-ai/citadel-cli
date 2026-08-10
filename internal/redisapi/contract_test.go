package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The tests in this file pin the client against the EXACT JSON bodies the
// aceteam platform routes emit today. Every fixture below is copied from the
// `NextResponse.json(...)` calls in the corresponding route:
//
//	app/api/fabric/redis/pubsub/publish/route.ts  -> { published: true }
//	app/api/fabric/redis/jobs/acknowledge/route.ts -> { acknowledged: <XACK > 0> }
//	app/api/fabric/redis/kv/route.ts (GET)         -> { key, value, ttl }
//	app/api/fabric/redis/kv/route.ts (POST)        -> { success: true }
//	app/api/fabric/redis/kv/route.ts (DELETE)      -> { deleted: <DEL > 0> }
//	app/api/fabric/redis/streams/add/route.ts      -> { success: true, messageId }
//
// citadel-cli #721: the client used to require a `success` field on the publish
// and acknowledge responses, which neither route has ever sent, and an `exists`
// field on the KV GET response, which that route has never sent either. Do not
// relax these fixtures to make a client change pass; change the client.

// newContractServer serves one canned JSON body for any request and records the
// request path so the test can assert the client hit the route it claims to.
func newContractServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotPath
}

func newContractClient(url string) *Client {
	return NewClient(ClientConfig{BaseURL: url, Token: "test-token"})
}

func TestPublishAcceptsRoutePublishedField(t *testing.T) {
	srv, gotPath := newContractServer(t, http.StatusOK, `{"published":true}`)
	client := newContractClient(srv.URL)

	err := client.Publish(context.Background(), "stream:v1:job-1", map[string]any{"hello": "world"})
	if err != nil {
		t.Fatalf("Publish should succeed on the route's real 200 body, got: %v", err)
	}
	if *gotPath != "/api/fabric/redis/pubsub/publish" {
		t.Errorf("unexpected path: %s", *gotPath)
	}
}

func TestPublishErrorCarriesStatusAndBody(t *testing.T) {
	// The 403 the route returns when the device key lacks device_redis:pubsub.
	srv, _ := newContractServer(t, http.StatusForbidden,
		`{"error":"Unauthorized - requires device_redis:pubsub scope"}`)
	client := newContractClient(srv.URL)

	err := client.Publish(context.Background(), "stream:v1:job-1", map[string]any{})
	if err == nil {
		t.Fatal("Publish should error on 403")
	}
	msg := err.Error()
	for _, want := range []string{"403", "/api/fabric/redis/pubsub/publish", "device_redis:pubsub"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q so one log line is enough to diagnose it", msg, want)
		}
	}
}

func TestPublishErrorCarriesStatusAndBodyForNonJSONError(t *testing.T) {
	// A gateway/proxy failure in front of the route: not JSON, no `error` key.
	srv, _ := newContractServer(t, http.StatusBadGateway, `<html>502 Bad Gateway</html>`)
	client := newContractClient(srv.URL)

	err := client.Publish(context.Background(), "stream:v1:job-1", map[string]any{})
	if err == nil {
		t.Fatal("Publish should error on 502")
	}
	msg := err.Error()
	for _, want := range []string{"502", "/api/fabric/redis/pubsub/publish", "Bad Gateway"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
}

func TestAcknowledgeJobAcceptsRouteAcknowledgedField(t *testing.T) {
	srv, gotPath := newContractServer(t, http.StatusOK, `{"acknowledged":true}`)
	client := newContractClient(srv.URL)

	err := client.AcknowledgeJob(context.Background(), AcknowledgeRequest{
		Queue: "jobs:v1:cpu-general", Group: "citadel-workers", MessageID: "1-0",
	})
	if err != nil {
		t.Fatalf("AcknowledgeJob should succeed on the route's real 200 body, got: %v", err)
	}
	if *gotPath != "/api/fabric/redis/jobs/acknowledge" {
		t.Errorf("unexpected path: %s", *gotPath)
	}
}

func TestAcknowledgeJobTreatsDuplicateAckAsSuccess(t *testing.T) {
	// XACK returns 0 when the message is already out of the PEL. ws_source acks
	// over WebSocket first and retries over HTTP when that fails, so a duplicate
	// ack is a routine outcome, not an error.
	srv, _ := newContractServer(t, http.StatusOK, `{"acknowledged":false}`)
	client := newContractClient(srv.URL)

	err := client.AcknowledgeJob(context.Background(), AcknowledgeRequest{
		Queue: "jobs:v1:cpu-general", Group: "citadel-workers", MessageID: "1-0",
	})
	if err != nil {
		t.Fatalf("a duplicate ack (acknowledged=false on 200) must not be an error, got: %v", err)
	}
}

func TestGetKeyParsesRouteBodyForExistingKey(t *testing.T) {
	srv, gotPath := newContractServer(t, http.StatusOK,
		`{"key":"job:cancelled:abc","value":"1","ttl":42}`)
	client := newContractClient(srv.URL)

	value, ttl, err := client.GetKey(context.Background(), "job:cancelled:abc")
	if err != nil {
		t.Fatalf("GetKey failed: %v", err)
	}
	if value != "1" {
		t.Errorf("value = %q, want %q", value, "1")
	}
	if ttl != 42 {
		t.Errorf("ttl = %d, want 42", ttl)
	}
	if *gotPath != "/api/fabric/redis/kv" {
		t.Errorf("unexpected path: %s", *gotPath)
	}
}

func TestGetKeyReportsMissingKeyFromNullValue(t *testing.T) {
	srv, _ := newContractServer(t, http.StatusOK,
		`{"key":"job:cancelled:abc","value":null,"ttl":-2}`)
	client := newContractClient(srv.URL)

	value, ttl, err := client.GetKey(context.Background(), "job:cancelled:abc")
	if err != nil {
		t.Fatalf("GetKey failed: %v", err)
	}
	if value != "" || ttl != -2 {
		t.Errorf("GetKey = (%q, %d), want (\"\", -2) for an absent key", value, ttl)
	}
}

func TestGetKeyHandlesEmptyStringValue(t *testing.T) {
	// An empty string is a real value; only JSON null means absent.
	srv, _ := newContractServer(t, http.StatusOK,
		`{"key":"device:state:org:x","value":"","ttl":-1}`)
	client := newContractClient(srv.URL)

	value, ttl, err := client.GetKey(context.Background(), "device:state:org:x")
	if err != nil {
		t.Fatalf("GetKey failed: %v", err)
	}
	if value != "" {
		t.Errorf("value = %q, want empty string", value)
	}
	if ttl != -1 {
		t.Errorf("ttl = %d, want -1 (exists, no expiry)", ttl)
	}
}

func TestIsJobCancelledReadsRouteBody(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		srv, _ := newContractServer(t, http.StatusOK,
			`{"key":"job:cancelled:abc","value":"1","ttl":3600}`)
		client := newContractClient(srv.URL)

		cancelled, err := client.IsJobCancelled(context.Background(), "abc")
		if err != nil {
			t.Fatalf("IsJobCancelled failed: %v", err)
		}
		if !cancelled {
			t.Error("IsJobCancelled = false, want true when the cancellation flag exists")
		}
	})

	t.Run("not cancelled", func(t *testing.T) {
		srv, _ := newContractServer(t, http.StatusOK,
			`{"key":"job:cancelled:abc","value":null,"ttl":-2}`)
		client := newContractClient(srv.URL)

		cancelled, err := client.IsJobCancelled(context.Background(), "abc")
		if err != nil {
			t.Fatalf("IsJobCancelled failed: %v", err)
		}
		if cancelled {
			t.Error("IsJobCancelled = true, want false when the flag is absent")
		}
	})
}

func TestSetKeyAcceptsRouteBody(t *testing.T) {
	srv, gotPath := newContractServer(t, http.StatusOK, `{"success":true}`)
	client := newContractClient(srv.URL)

	if err := client.SetKey(context.Background(), "job:x:status", map[string]any{"status": "ok"}, 60); err != nil {
		t.Fatalf("SetKey failed: %v", err)
	}
	if *gotPath != "/api/fabric/redis/kv" {
		t.Errorf("unexpected path: %s", *gotPath)
	}
}

func TestDeleteKeyParsesRouteBody(t *testing.T) {
	srv, _ := newContractServer(t, http.StatusOK, `{"deleted":true}`)
	client := newContractClient(srv.URL)

	deleted, err := client.DeleteKey(context.Background(), "config:cache:x")
	if err != nil {
		t.Fatalf("DeleteKey failed: %v", err)
	}
	if !deleted {
		t.Error("DeleteKey = false, want true")
	}
}

func TestStreamAddAcceptsRouteBody(t *testing.T) {
	srv, gotPath := newContractServer(t, http.StatusOK,
		`{"success":true,"messageId":"1754400000000-0"}`)
	client := newContractClient(srv.URL)

	err := client.StreamAdd(context.Background(), "node:status:stream",
		map[string]string{"node": "n1"}, 10000)
	if err != nil {
		t.Fatalf("StreamAdd failed: %v", err)
	}
	if *gotPath != "/api/fabric/redis/streams/add" {
		t.Errorf("unexpected path: %s", *gotPath)
	}
}

func TestConsumeJobParsesRouteBody(t *testing.T) {
	srv, gotPath := newContractServer(t, http.StatusOK,
		`{"messages":[{"id":"1-0","data":{"jobId":"job-1","type":"SHELL_COMMAND","payload":"{\"cmd\":\"ls\"}","enqueuedAt":"2026-08-05T00:00:00Z","rayId":"ray-1"}}]}`)
	client := newContractClient(srv.URL)

	job, err := client.ConsumeJob(context.Background(), ConsumeRequest{
		Queue: "jobs:v1:cpu-general", Group: "citadel-workers", Consumer: "c1", BlockMs: 10,
	})
	if err != nil {
		t.Fatalf("ConsumeJob failed: %v", err)
	}
	if job == nil {
		t.Fatal("ConsumeJob returned nil job")
	}
	if job.JobID != "job-1" || job.Type != "SHELL_COMMAND" || job.MessageID != "1-0" {
		t.Errorf("unexpected job: %+v", job)
	}
	if *gotPath != "/api/fabric/redis/jobs/consume" {
		t.Errorf("unexpected path: %s", *gotPath)
	}
}
