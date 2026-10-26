package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const bindingFixtureOrg = "11111111-2222-4333-8444-abcdefabcdef"
const bindingFixtureBody = "{\"state\":\"confirmed\",\"org_id\":\"11111111-2222-4333-8444-abcdefabcdef\",\"node\":{\"domain\":\"headscale\",\"id\":\"42\"},\"observed_at\":\"2026-10-25T12:00:00.123Z\"}"

func isolateCurrentBindingHome(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		path := filepath.Join(root, key)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
}

func bindingRequest() DeviceBindingRequest {
	return DeviceBindingRequest{APIBaseURL: "https://api.invalid/tenant", Bearer: "fixture-secret", OrgID: bindingFixtureOrg, NodeID: "42"}
}

type bindingRoundTripper func(*http.Request) (*http.Response, error)

func (f bindingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type bindingTrackedBody struct {
	io.Reader
	reads  int
	closed bool
	cancel context.CancelFunc
}

func (b *bindingTrackedBody) Read(p []byte) (int, error) {
	b.reads++
	if b.cancel != nil {
		b.cancel()
	}
	return b.Reader.Read(p)
}
func (b *bindingTrackedBody) Close() error { b.closed = true; return nil }

func bindingResponse(status int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status, Body: body,
		Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
	}
}

func TestCurrentBindingUsesOnlyCapturedBearerAndExactBasePath(t *testing.T) {
	isolateCurrentBindingHome(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/tenant"+CurrentDeviceBindingPath ||
			r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer fixture-secret" ||
			r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Accept") != "application/json" {
			t.Error("lookup did not use the exact current-key self-only request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, bindingFixtureBody)
	}))
	defer server.Close()
	input := bindingRequest()
	input.APIBaseURL = server.URL + "/tenant/"
	if got := LookupCurrentDeviceBinding(context.Background(), input); got != BindingConfirmed || calls.Load() != 1 {
		t.Fatalf("state=%v calls=%d; want one confirmed observation", got, calls.Load())
	}
}

func TestCurrentBindingRefusalsAreStatusOnlyAndBodiesUnread(t *testing.T) {
	isolateCurrentBindingHome(t)
	for _, test := range []struct {
		status int
		want   DeviceBindingState
	}{
		{401, BindingRefused}, {403, BindingRefused},
		{404, BindingUnsupported}, {405, BindingUnsupported},
		{429, BindingUnknown}, {500, BindingUnknown}, {502, BindingUnknown}, {503, BindingUnknown},
		{301, BindingUnknown}, {302, BindingUnknown}, {307, BindingUnknown}, {308, BindingUnknown},
	} {
		body := &bindingTrackedBody{Reader: strings.NewReader("fixture-secret private-refusal-body")}
		calls := 0
		client := &http.Client{Transport: bindingRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			response := bindingResponse(test.status, body)
			response.Header.Set("Location", "https://redirect.invalid/secret-target")
			return response, nil
		})}
		if got := lookupCurrentDeviceBinding(context.Background(), bindingRequest(), client); got != test.want {
			t.Fatalf("HTTP %d: state=%v want=%v", test.status, got, test.want)
		}
		if calls != 1 || body.reads != 0 || !body.closed {
			t.Fatalf("HTTP %d: calls=%d reads=%d closed=%t", test.status, calls, body.reads, body.closed)
		}
	}
}

func TestCurrentBindingNeverForwardsBearerOnRedirect(t *testing.T) {
	isolateCurrentBindingHome(t)
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, bindingFixtureBody)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	input := bindingRequest()
	input.APIBaseURL = origin.URL
	if got := LookupCurrentDeviceBinding(context.Background(), input); got != BindingUnknown || forwarded.Load() != 0 {
		t.Fatalf("redirect state=%v forwarded=%d", got, forwarded.Load())
	}
}

func TestCurrentBindingRequiresStrictCompleteIdentitySchema(t *testing.T) {
	isolateCurrentBindingHome(t)
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing state", func(v map[string]any) { delete(v, "state") }},
		{"unknown state", func(v map[string]any) { v["state"] = "linked" }},
		{"null state", func(v map[string]any) { v["state"] = nil }},
		{"wrong case", func(v map[string]any) { v["STATE"] = v["state"]; delete(v, "state") }},
		{"extra field", func(v map[string]any) { v["key_id"] = "not-needed" }},
		{"missing org", func(v map[string]any) { delete(v, "org_id") }},
		{"wrong org", func(v map[string]any) { v["org_id"] = "22222222-3333-4444-8555-aaaaaaaaaaaa" }},
		{"uppercase org", func(v map[string]any) { v["org_id"] = strings.ToUpper(bindingFixtureOrg) }},
		{"null org", func(v map[string]any) { v["org_id"] = nil }},
		{"missing node", func(v map[string]any) { delete(v, "node") }},
		{"null node", func(v map[string]any) { v["node"] = nil }},
		{"wrong domain", func(v map[string]any) { v["node"].(map[string]any)["domain"] = "fabric" }},
		{"wrong node", func(v map[string]any) { v["node"].(map[string]any)["id"] = "43" }},
		{"numeric node", func(v map[string]any) { v["node"].(map[string]any)["id"] = 42 }},
		{"zero node", func(v map[string]any) { v["node"].(map[string]any)["id"] = "0" }},
		{"padded node", func(v map[string]any) { v["node"].(map[string]any)["id"] = "042" }},
		{"signed node", func(v map[string]any) { v["node"].(map[string]any)["id"] = "+42" }},
		{"missing node domain", func(v map[string]any) { delete(v["node"].(map[string]any), "domain") }},
		{"extra node field", func(v map[string]any) { v["node"].(map[string]any)["hostname"] = "not-proof" }},
		{"missing time", func(v map[string]any) { delete(v, "observed_at") }},
		{"null time", func(v map[string]any) { v["observed_at"] = nil }},
		{"bad time", func(v map[string]any) { v["observed_at"] = "not-a-time" }},
		{"wrong time precision", func(v map[string]any) { v["observed_at"] = "2026-10-25T12:00:00Z" }},
		{"time offset", func(v map[string]any) { v["observed_at"] = "2026-10-25T12:00:00.123+00:00" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]any
			if json.Unmarshal([]byte(bindingFixtureBody), &value) != nil {
				t.Fatal("invalid fixture")
			}
			test.change(value)
			body, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if decodeCurrentBinding(body, bindingRequest()) != BindingUnknown {
				t.Fatal("malformed or mismatched evidence was confirmed")
			}
		})
	}
	for _, body := range []string{
		"", "null", "[]", "<html>fixture-secret</html>", bindingFixtureBody + " {}",
		strings.Replace(bindingFixtureBody, "\"state\":", "\"state\":\"confirmed\",\"state\":", 1),
		strings.Replace(bindingFixtureBody, "\"id\":\"42\"", "\"id\":\"42\",\"id\":\"42\"", 1),
		"{\"state\":\"not_linked\",\"observed_at\":\"2026-10-25T12:00:00.123Z\",\"org_id\":null}",
	} {
		if decodeCurrentBinding([]byte(body), bindingRequest()) != BindingUnknown {
			t.Fatal("nonobject, duplicate or trailing JSON was accepted")
		}
	}
	if decodeCurrentBinding([]byte("{\"state\":\"not_linked\",\"observed_at\":\"2026-10-25T12:00:00.123Z\"}"), bindingRequest()) != BindingNotLinked {
		t.Fatal("strict not_linked control refused")
	}
	// Server wall time is informational; acceptance comes from this request's
	// monotonic budget, even on hosts with an absent or incorrect RTC.
	for _, at := range []string{"1900-01-01T00:00:00.000Z", "2099-12-31T23:59:59.999Z"} {
		body := strings.Replace(bindingFixtureBody, "2026-10-25T12:00:00.123Z", at, 1)
		if decodeCurrentBinding([]byte(body), bindingRequest()) != BindingConfirmed {
			t.Fatal("informational wall time became a freshness authority")
		}
	}
}

func TestCurrentBindingRejectsInvalidInputsBeforeHTTP(t *testing.T) {
	isolateCurrentBindingHome(t)
	for _, change := range []func(*DeviceBindingRequest){
		func(r *DeviceBindingRequest) { r.APIBaseURL = "" },
		func(r *DeviceBindingRequest) { r.APIBaseURL = "https://api.invalid?org_id=other" },
		func(r *DeviceBindingRequest) { r.APIBaseURL = "https://api.invalid?" },
		func(r *DeviceBindingRequest) { r.APIBaseURL = "https://api.invalid#" },
		func(r *DeviceBindingRequest) { r.APIBaseURL = "https://user:secret@api.invalid" },
		func(r *DeviceBindingRequest) { r.APIBaseURL = "file:///tmp/fixture" },
		func(r *DeviceBindingRequest) { r.Bearer = "" },
		func(r *DeviceBindingRequest) { r.Bearer = "bad\nheader" },
		func(r *DeviceBindingRequest) { r.OrgID = "hostname" },
		func(r *DeviceBindingRequest) { r.NodeID = "" },
		func(r *DeviceBindingRequest) { r.NodeID = "node-42" },
		func(r *DeviceBindingRequest) { r.NodeID = "0" },
		func(r *DeviceBindingRequest) { r.NodeID = "042" },
	} {
		input := bindingRequest()
		change(&input)
		client := &http.Client{Transport: bindingRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid lookup inputs reached the transport")
			return nil, nil
		})}
		if lookupCurrentDeviceBinding(context.Background(), input, client) != BindingUnknown {
			t.Fatal("invalid current-input snapshot was accepted")
		}
	}
}

func TestCurrentBindingBoundsBodyAndRequiresUncacheableJSON(t *testing.T) {
	isolateCurrentBindingHome(t)
	for _, test := range []struct {
		name, body, contentType, cache string
		want                           DeviceBindingState
	}{
		{"at bound", bindingFixtureBody + strings.Repeat(" ", maxCurrentBindingBytes-len(bindingFixtureBody)), "application/json", "no-store", BindingConfirmed},
		{"over bound", bindingFixtureBody + strings.Repeat(" ", maxCurrentBindingBytes+1-len(bindingFixtureBody)), "application/json", "no-store", BindingUnknown},
		{"html type", bindingFixtureBody, "text/html", "no-store", BindingUnknown},
		{"missing type", bindingFixtureBody, "", "no-store", BindingUnknown},
		{"missing cache directive", bindingFixtureBody, "application/json", "", BindingUnknown},
		{"cacheable", bindingFixtureBody, "application/json", "public, max-age=60", BindingUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &bindingTrackedBody{Reader: strings.NewReader(test.body)}
			client := &http.Client{Transport: bindingRoundTripper(func(*http.Request) (*http.Response, error) {
				response := bindingResponse(200, body)
				response.Header.Set("Content-Type", test.contentType)
				response.Header.Set("Cache-Control", test.cache)
				return response, nil
			})}
			if got := lookupCurrentDeviceBinding(context.Background(), bindingRequest(), client); got != test.want || !body.closed {
				t.Fatalf("state=%v want=%v closed=%t", got, test.want, body.closed)
			}
		})
	}
}

func TestCurrentBindingCancellationAndTransportErrorsNeverConfirm(t *testing.T) {
	isolateCurrentBindingHome(t)
	if CurrentDeviceBindingTimeout != 5*time.Second {
		t.Fatal("current-binding request budget changed without contract review")
	}
	for _, when := range []string{"before request", "during headers", "during body", "transport error"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			if when == "before request" {
				cancel()
			}
			client := &http.Client{Transport: bindingRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > CurrentDeviceBindingTimeout {
					t.Error("lookup omitted the bounded monotonic request deadline")
				}
				if when == "transport error" {
					return nil, errors.New("fixture-secret raw transport failure")
				}
				body := &bindingTrackedBody{Reader: strings.NewReader(bindingFixtureBody)}
				if when == "during headers" {
					cancel()
				} else if when == "during body" {
					body.cancel = cancel
				}
				return bindingResponse(200, body), nil
			})}
			if lookupCurrentDeviceBinding(ctx, bindingRequest(), client) != BindingUnknown {
				t.Fatal("cancelled/failed observation was confirmed")
			}
			wantCalls := 1
			if when == "before request" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d; request retried or cancellation ignored", calls, wantCalls)
			}
		})
	}
}
