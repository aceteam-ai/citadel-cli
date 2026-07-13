package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/proto"
)

func TestGetDesiredStateProtoRoundTripAndAuth(t *testing.T) {
	want := &fabricpb.DesiredState{
		Revision: "rev-5",
		NodeId:   "node-42",
		Modules: []*fabricpb.DesiredModule{
			{Source: "owner/repo@^1.2", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING},
		},
	}
	body, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var gotPath, gotAuth, gotAccept, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "device-token-xyz"})
	raw, err := c.GetDesiredState(context.Background(), "node-42")
	if err != nil {
		t.Fatalf("GetDesiredState: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/fabric/nodes/node-42/desired-state" {
		t.Errorf("path = %q, want /api/fabric/nodes/node-42/desired-state", gotPath)
	}
	if gotAuth != "Bearer device-token-xyz" {
		t.Errorf("auth = %q, want Bearer device-token-xyz", gotAuth)
	}
	if gotAccept != "application/octet-stream" {
		t.Errorf("accept = %q, want application/octet-stream", gotAccept)
	}

	var got fabricpb.DesiredState
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode returned body: %v", err)
	}
	if got.GetRevision() != "rev-5" || len(got.GetModules()) != 1 {
		t.Fatalf("round-trip mismatch: %+v", &got)
	}
}

func TestGetDesiredStateErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no desired state for node"))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	if _, err := c.GetDesiredState(context.Background(), "node-1"); err == nil {
		t.Fatal("want error on non-2xx status")
	}
}

func TestDesiredStatePathHelper(t *testing.T) {
	if got := DesiredStatePath("abc"); got != "/api/fabric/nodes/abc/desired-state" {
		t.Fatalf("DesiredStatePath = %q", got)
	}
}
