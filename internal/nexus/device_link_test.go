package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLinkRegisteredNodeUsesCurrentDeviceGrantAndIdentity(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost || r.URL.Path != "/api/fabric/device-config/current-grant/link" || r.Header.Get("Authorization") != "" {
			t.Errorf("link must use the current grant without an unrelated bearer")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["nodeId"] != "42" {
			t.Errorf("link body must identify only the registered Headscale node: %v", body)
		}
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer srv.Close()
	if err := NewDeviceAuthClient(srv.URL).LinkRegisteredNode(context.Background(), "current-grant", "42"); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("link calls = %d", called)
	}
}

func TestLinkRegisteredNodeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing registration proof", 409, `{"error":"private-grant private-token"}`},
		{"mismatched registration proof", 403, `{"error":"private-grant private-token"}`},
		{"wrong node", 403, `{"error":"private-grant private-token"}`},
		{"expired enrollment", 404, `{"error":"private-grant private-token"}`},
		{"server unavailable", 503, `{"error":"private-grant private-token"}`},
		{"unconfirmed success", 200, `{"success":false}`},
		{"malformed success", 200, `{"success":"private-token"}`},
		{"trailing response", 200, `{"success":true} {}`},
		{"oversized response", 200, `{"success":true}` + strings.Repeat(" ", maxDeviceAuthResponseBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			err := NewDeviceAuthClient(srv.URL).LinkRegisteredNode(context.Background(), "private-grant", "42")
			if err == nil {
				t.Fatal("link must fail closed")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("link diagnostic exposed grant or credential")
			}
		})
	}
}

func TestLinkRegisteredNodeRejectsMissingIdentityBeforeRequest(t *testing.T) {
	for _, args := range [][2]string{{"", "42"}, {"grant", ""}, {"grant", "hostname"}, {"grant", "100.64.0.1"}, {"grant", "0"}} {
		if err := NewDeviceAuthClient("http://127.0.0.1:1").LinkRegisteredNode(context.Background(), args[0], args[1]); err == nil {
			t.Fatal("missing current identity must fail")
		}
	}
}

func TestLinkRegisteredNodeNeverFollowsRedirect(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	err := NewDeviceAuthClient(redirect.URL).LinkRegisteredNode(context.Background(), "private-grant", "42")
	if err == nil || forwarded {
		t.Fatal("redirect must fail without forwarding enrollment credentials")
	}
}
