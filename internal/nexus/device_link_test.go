package nexus

import (
	"context"
	"encoding/json"
	"errors"
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
		name    string
		status  int
		body    string
		refusal bool
	}{
		{"missing registration proof", 409, `{"error":"private-grant private-token"}`, true},
		{"mismatched registration proof", 403, `{"error":"private-grant private-token"}`, true},
		{"wrong node", 403, `{"error":"private-grant private-token"}`, true},
		{"expired enrollment", 404, `{"error":"private-grant private-token"}`, true},
		{"server unavailable", 503, `{"error":"private-grant private-token"}`, false},
		{"bad gateway", 502, `<html>private-grant</html>`, false},
		{"unconfirmed success", 200, `{"success":false}`, true},
		{"malformed success", 200, `{"success":"private-token"}`, false},
		{"missing success", 200, `{}`, false},
		{"trailing response", 200, `{"success":true} {}`, false},
		{"oversized response", 200, `{"success":true}` + strings.Repeat(" ", maxDeviceAuthResponseBytes), false},
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
			var refused *DeviceLinkRefusedError
			if errors.As(err, &refused) != tc.refusal {
				t.Fatalf("refusal classification = %v, want %v (%v)", !tc.refusal, tc.refusal, err)
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

func TestServerVersionReadsVersionField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/version" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"version":"1.520.0","sha":"abc","service":"web"}`)
	}))
	defer srv.Close()
	v, err := NewDeviceAuthClient(srv.URL).ServerVersion(context.Background())
	if err != nil || v != "1.520.0" {
		t.Fatalf("version = %q, %v", v, err)
	}
}

func TestServerVersionUnreadableIsAnError(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"not found":     func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"empty version": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":""}`) },
		"not json":      func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>`) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			if v, err := NewDeviceAuthClient(srv.URL).ServerVersion(context.Background()); err == nil {
				t.Fatalf("unreadable version must be an error, got %q", v)
			}
		})
	}
}
