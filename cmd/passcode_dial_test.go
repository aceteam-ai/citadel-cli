// cmd/passcode_dial_test.go
//
// Pins the #754 discrimination between the terminal endpoint's two distinct
// HTTP 401 causes (internal/terminal/server.go): a rejected/invalid auth
// token (resolveAuth) vs. the passcode gate (PasscodeVerifier). Both return
// status 401, so the response body is the only signal. See
// isPasscodeRequiredResponse in cmd/connect_shell.go. Getting this wrong in
// either direction is a real regression: treating every 401 as
// passcode-required would turn a bad --token into three passcode prompts and
// a misleading "passcode rejected" error; treating the real passcode gate as
// a generic auth failure would send an operator to check their token instead
// of prompting for the passcode #754 actually needs.
package cmd

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestIsPasscodeRequiredResponse(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		want bool
	}{
		{
			name: "passcode gate by error text (pre-#753 shape, current main)",
			resp: jsonResp(401, `{"error":"node passcode required","status":401}`),
			want: true,
		},
		{
			name: "passcode gate with reason field (citadel#753)",
			resp: jsonResp(401, `{"error":"node passcode required","status":401,"reason":"passcode_invalid"}`),
			want: true,
		},
		{
			name: "passcode_not_set reason still recognized even if error text ever drifts",
			resp: jsonResp(401, `{"error":"something else","status":401,"reason":"passcode_not_set"}`),
			want: true,
		},
		{
			name: "rejected auth token is NOT passcode-required",
			resp: jsonResp(401, `{"error":"invalid token","status":401}`),
			want: false,
		},
		{
			name: "generic authentication failed is NOT passcode-required",
			resp: jsonResp(401, `{"error":"authentication failed","status":401}`),
			want: false,
		},
		{
			name: "malformed body treated as not-passcode-required, not a crash",
			resp: jsonResp(401, `not json`),
			want: false,
		},
		{
			name: "nil response",
			resp: nil,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPasscodeRequiredResponse(c.resp); got != c.want {
				t.Errorf("isPasscodeRequiredResponse() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRemoteShellDialError_PasscodeGateClassifiedForRetry(t *testing.T) {
	resp := jsonResp(401, `{"error":"node passcode required","status":401}`)
	err := remoteShellDialError(errors.New("bad handshake"), resp, "gpu-node-1", "100.64.0.5:7860")

	var tdErr *terminalDialError
	if !errors.As(err, &tdErr) {
		t.Fatalf("remoteShellDialError() = %v, want a *terminalDialError", err)
	}
	if tdErr.kind != terminalDialErrPasscode {
		t.Errorf("kind = %v, want terminalDialErrPasscode", tdErr.kind)
	}
}

func TestRemoteShellDialError_RejectedTokenNotClassifiedAsPasscode(t *testing.T) {
	resp := jsonResp(401, `{"error":"invalid token","status":401}`)
	err := remoteShellDialError(errors.New("bad handshake"), resp, "gpu-node-1", "100.64.0.5:7860")

	var tdErr *terminalDialError
	if errors.As(err, &tdErr) {
		t.Fatalf("remoteShellDialError() = %v, want a plain (non-terminalDialError) auth-rejected error, not classified as passcode or unreachable", err)
	}
	if err == nil {
		t.Fatal("remoteShellDialError() = nil, want an error")
	}
}

func TestRemoteShellDialError_ConnRefusedClassifiedForFallback(t *testing.T) {
	err := remoteShellDialError(errors.New("dial tcp 100.64.0.5:7860: connect: connection refused"), nil, "gpu-node-1", "100.64.0.5:7860")

	var tdErr *terminalDialError
	if !errors.As(err, &tdErr) {
		t.Fatalf("remoteShellDialError() = %v, want a *terminalDialError", err)
	}
	if tdErr.kind != terminalDialErrUnreachable {
		t.Errorf("kind = %v, want terminalDialErrUnreachable", tdErr.kind)
	}
}

func TestRemoteShellDialError_OtherTransportFailureNotClassifiedAsUnreachable(t *testing.T) {
	err := remoteShellDialError(errors.New("dial tcp 100.64.0.5:7860: i/o timeout"), nil, "gpu-node-1", "100.64.0.5:7860")

	var tdErr *terminalDialError
	if errors.As(err, &tdErr) {
		t.Fatalf("remoteShellDialError() = %v, want a plain error (not a fallback trigger) for a non-refused transport failure", err)
	}
	if err == nil {
		t.Fatal("remoteShellDialError() = nil, want an error")
	}
}

// TestTerminalWSURL pins the #754 wire-level requirement: the passcode is
// forwarded as a "?passcode=" query parameter on the terminal WebSocket
// upgrade URL, present ONLY when non-empty. The empty case matters as much
// as the present case: a node with no passcode configured must see a
// byte-identical request to before #754, or internal/terminal/server.go's
// gate (only active when config.PasscodeVerifier != nil, independent of the
// query string) would still work, but a regression here would be invisible
// to every routing test in this package (they all stub terminalAttemptFn).
func TestTerminalWSURL(t *testing.T) {
	cases := []struct {
		name            string
		token, passcode string
		wantHasPasscode bool
		wantPasscodeVal string
		wantHasToken    bool
	}{
		{name: "neither set", token: "", passcode: "", wantHasPasscode: false, wantHasToken: false},
		{name: "passcode only", token: "", passcode: "hunter2", wantHasPasscode: true, wantPasscodeVal: "hunter2", wantHasToken: false},
		{name: "token only", token: "tok_abc", passcode: "", wantHasPasscode: false, wantHasToken: true},
		{name: "both set", token: "tok_abc", passcode: "hunter2", wantHasPasscode: true, wantPasscodeVal: "hunter2", wantHasToken: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := terminalWSURL("100.64.0.5:7860", c.token, c.passcode)
			q := u.Query()

			if _, has := q["passcode"]; has != c.wantHasPasscode {
				t.Errorf("passcode key present = %v, want %v (query=%q)", has, c.wantHasPasscode, u.RawQuery)
			}
			if c.wantHasPasscode && q.Get("passcode") != c.wantPasscodeVal {
				t.Errorf("passcode value = %q, want %q", q.Get("passcode"), c.wantPasscodeVal)
			}
			if _, has := q["token"]; has != c.wantHasToken {
				t.Errorf("token key present = %v, want %v (query=%q)", has, c.wantHasToken, u.RawQuery)
			}
			if u.Scheme != "ws" || u.Host != "100.64.0.5:7860" || u.Path != "/terminal" {
				t.Errorf("URL = %+v, want scheme=ws host=100.64.0.5:7860 path=/terminal", u)
			}
		})
	}
}
