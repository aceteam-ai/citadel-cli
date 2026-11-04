package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type deviceLinkDiagnosticTransport func(*http.Request) (*http.Response, error)

func (f deviceLinkDiagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type deviceLinkDiagnosticOpaqueError struct{ rendered bool }

func (e *deviceLinkDiagnosticOpaqueError) Error() string {
	e.rendered = true
	panic("private-grant private-token must never be rendered")
}

type deviceLinkDiagnosticBody struct {
	reader  io.Reader
	readErr error
	reads   int
	closes  int
}

func (b *deviceLinkDiagnosticBody) Read(p []byte) (int, error) {
	b.reads++
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.reader.Read(p)
}

func (b *deviceLinkDiagnosticBody) Close() error {
	b.closes++
	return nil
}

func TestDeviceLinkHTTPDiagnosticsUseOnlyStatusAndNeverReadErrorBodies(t *testing.T) {
	for _, tc := range []struct {
		status  int
		kind    DeviceLinkFailureKind
		refused bool
	}{
		{http.StatusForbidden, DeviceLinkRefused, true},
		{http.StatusGone, DeviceLinkRefused, true},
		{http.StatusServiceUnavailable, DeviceLinkHTTPServer, false},
		{http.StatusBadGateway, DeviceLinkHTTPServer, false},
		{http.StatusCreated, DeviceLinkHTTPStatus, false},
		{http.StatusTemporaryRedirect, DeviceLinkHTTPStatus, false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			opaque := &deviceLinkDiagnosticOpaqueError{}
			body := &deviceLinkDiagnosticBody{readErr: opaque}
			client := NewDeviceAuthClient("https://private-origin.invalid")
			client.httpClient.Transport = deviceLinkDiagnosticTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body, Request: r}, nil
			})
			err := client.LinkRegisteredNode(context.Background(), "private-grant", "42")
			kind, status := DeviceLinkFailureDetails(err)
			var refusal *DeviceLinkRefusedError
			if err == nil || kind != tc.kind || status != tc.status || errors.As(err, &refusal) != tc.refused {
				t.Fatal("HTTP status lost its safe category or changed definite-refusal authority")
			}
			if body.reads != 0 || body.closes != 1 || opaque.rendered || errors.Unwrap(err) != nil {
				t.Fatal("HTTP failure read, rendered, retained, or failed to close its opaque body")
			}
			text := err.Error()
			if !strings.Contains(text, fmt.Sprintf("HTTP %d", tc.status)) || strings.Contains(text, "private-") {
				t.Fatal("status diagnostic was missing or exposed an opaque server value")
			}
		})
	}
}

func TestDeviceLinkTransportAndMalformedDiagnosticsDiscardOpaqueErrors(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("body-read-error-%t", malformed), func(t *testing.T) {
			opaque := &deviceLinkDiagnosticOpaqueError{}
			body := &deviceLinkDiagnosticBody{readErr: opaque}
			client := NewDeviceAuthClient("https://private-origin.invalid")
			client.httpClient.Transport = deviceLinkDiagnosticTransport(func(r *http.Request) (*http.Response, error) {
				if !malformed {
					return nil, opaque
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
			})
			err := client.LinkRegisteredNode(context.Background(), "private-grant", "42")
			wantKind, wantStatus := DeviceLinkTransport, 0
			if malformed {
				wantKind, wantStatus = DeviceLinkMalformedResponse, http.StatusOK
			}
			kind, status := DeviceLinkFailureDetails(err)
			if err == nil || kind != wantKind || status != wantStatus || strings.Contains(err.Error(), "private-") || opaque.rendered || errors.Unwrap(err) != nil {
				t.Fatal("opaque transport/body failure was rendered, retained, or misclassified")
			}
			if malformed && (body.reads != 1 || body.closes != 1) {
				t.Fatal("malformed response did not close its bounded reader")
			}
		})
	}
}

func TestDeviceLinkResponseCategoriesPreserveConfirmationContract(t *testing.T) {
	for _, tc := range []struct {
		body string
		kind DeviceLinkFailureKind
	}{
		{`{"success":false}`, DeviceLinkRefused},
		{`{"success":"private-token"}`, DeviceLinkMalformedResponse},
		{`{}`, DeviceLinkMalformedResponse},
		{`{"success":true} {}`, DeviceLinkMalformedResponse},
		{`{"success":true}` + strings.Repeat(" ", maxDeviceAuthResponseBytes), DeviceLinkMalformedResponse},
	} {
		client := NewDeviceAuthClient("https://private-origin.invalid")
		body := &deviceLinkDiagnosticBody{reader: strings.NewReader(tc.body)}
		client.httpClient.Transport = deviceLinkDiagnosticTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
		})
		err := client.LinkRegisteredNode(context.Background(), "private-grant", "42")
		kind, status := DeviceLinkFailureDetails(err)
		if err == nil || kind != tc.kind || status != http.StatusOK || body.closes != 1 || strings.Contains(err.Error(), "private-") {
			t.Fatal("response category changed confirmation, size, or single-JSON requirements")
		}
	}
}

func TestDeviceLinkDiagnosticValidatesKindsStatusesAndLocalPreconditions(t *testing.T) {
	opaque := &deviceLinkDiagnosticOpaqueError{}
	var nilFailure *DeviceLinkFailure
	var nilRefusal *DeviceLinkRefusedError
	for _, err := range []error{
		nil, opaque, nilFailure, nilRefusal,
		NewDeviceLinkFailure(DeviceLinkFailureKind("private-grant"), 503),
		NewDeviceLinkFailure(DeviceLinkHTTPServer, 403),
		NewDeviceLinkFailure(DeviceLinkTransport, 503),
		NewDeviceLinkFailure(DeviceLinkRefused, http.StatusForbidden),
		&DeviceLinkFailure{kind: DeviceLinkFailureKind("private-token"), statusCode: 503},
		&DeviceLinkFailure{kind: DeviceLinkRefused, statusCode: http.StatusForbidden},
		&DeviceLinkFailure{kind: DeviceLinkHTTPServer, statusCode: 999},
		&DeviceLinkRefusedError{StatusCode: 503},
	} {
		kind, status := DeviceLinkFailureDetails(err)
		if kind != DeviceLinkUnconfirmed || status != 0 || DeviceLinkDiagnostic(err) != "category=unconfirmed" || opaque.rendered {
			t.Fatal("unrecognized diagnostic authority escaped the closed vocabulary")
		}
		switch err.(type) {
		case *DeviceLinkFailure, *DeviceLinkRefusedError:
			if text := err.Error(); strings.Contains(text, "private-") || strings.Contains(text, "HTTP") {
				t.Fatal("typed error rendering bypassed validation of kind/status fields")
			}
		}
	}
	for _, tc := range []struct {
		base  string
		grant string
		node  string
	}{
		{"https://example.invalid", "", "42"},
		{"https://example.invalid", "private-grant", "hostname"},
		{"://private-origin", "private-grant", "42"},
	} {
		client := NewDeviceAuthClient(tc.base)
		client.httpClient.Transport = deviceLinkDiagnosticTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid local preconditions must fail before transport")
			return nil, nil
		})
		err := client.LinkRegisteredNode(context.Background(), tc.grant, tc.node)
		kind, status := DeviceLinkFailureDetails(err)
		if err == nil || kind != DeviceLinkLocalPrecondition || status != 0 || strings.Contains(err.Error(), "private-") {
			t.Fatal("local failure was mislabeled as transport or exposed input")
		}
	}
}
