package nexus

import (
	"net/http"
	"strings"
	"testing"
)

const testCSRPEM = "-----BEGIN CERTIFICATE REQUEST-----\nMIIBTESTCSRBYTES\n-----END CERTIFICATE REQUEST-----\n"

// TestCheckTokenWithCSR_SendsCSR proves a CSR-carrying single poll puts the CSR
// in the request body (the non-TTY/TTY paths both route through this).
func TestCheckTokenWithCSR_SendsCSR(t *testing.T) {
	mock := StartMockDeviceAuthServer(1) // immediate success
	defer mock.Close()
	client := NewDeviceAuthClient(mock.URL())

	if _, err := client.CheckTokenWithCSR("dev-code", testCSRPEM); err != nil {
		t.Fatalf("CheckTokenWithCSR: %v", err)
	}

	csrs := mock.TokenRequestCSRs()
	if len(csrs) != 1 {
		t.Fatalf("expected 1 token request, got %d", len(csrs))
	}
	if csrs[0] != testCSRPEM {
		t.Errorf("csr_pem = %q, want %q", csrs[0], testCSRPEM)
	}
	if !strings.Contains(mock.TokenRequestBodies()[0], "csr_pem") {
		t.Errorf("request body missing csr_pem: %s", mock.TokenRequestBodies()[0])
	}
}

// TestCheckToken_NoCSRByteIdentical pins backward compatibility: the legacy
// no-CSR path omits csr_pem entirely (omitempty), so a legacy request body is
// byte-identical to before #1062.
func TestCheckToken_NoCSRByteIdentical(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()
	client := NewDeviceAuthClient(mock.URL())

	if _, err := client.CheckToken("dev-code"); err != nil {
		t.Fatalf("CheckToken: %v", err)
	}

	bodies := mock.TokenRequestBodies()
	if len(bodies) != 1 {
		t.Fatalf("expected 1 token request, got %d", len(bodies))
	}
	want := `{"device_code":"dev-code","grant_type":"urn:ietf:params:oauth:grant-type:device_code"}`
	if strings.TrimSpace(bodies[0]) != want {
		t.Errorf("legacy request body = %q, want %q", strings.TrimSpace(bodies[0]), want)
	}
	if strings.Contains(bodies[0], "csr_pem") {
		t.Errorf("legacy request body unexpectedly carries csr_pem: %s", bodies[0])
	}
}

// TestPollForTokenWithCSR_SameCSRAcrossRetries proves the SAME CSR is sent on
// every poll retry (pending, pending, ..., success).
func TestPollForTokenWithCSR_SameCSRAcrossRetries(t *testing.T) {
	const pollsUntilSuccess = 3
	mock := StartMockDeviceAuthServer(pollsUntilSuccess)
	defer mock.Close()
	client := NewDeviceAuthClient(mock.URL())

	token, err := client.PollForTokenWithCSR("dev-code", 0 /* fast */, testCSRPEM)
	if err != nil {
		t.Fatalf("PollForTokenWithCSR: %v", err)
	}
	if token.Authkey == "" {
		t.Fatal("expected an authkey on success")
	}

	csrs := mock.TokenRequestCSRs()
	if len(csrs) != pollsUntilSuccess {
		t.Fatalf("expected %d token requests, got %d", pollsUntilSuccess, len(csrs))
	}
	for i, got := range csrs {
		if got != testCSRPEM {
			t.Errorf("retry %d csr_pem = %q, want %q (must be identical across retries)", i, got, testCSRPEM)
		}
	}
}

// TestTokenResponse_ParsesEnrollmentBundle confirms the token response decodes
// the additive leaf_pem/chain_pem/node_uid fields.
func TestTokenResponse_ParsesEnrollmentBundle(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()
	mock.SetEnrollmentBundle("LEAF", "CHAIN", "uid-123")
	client := NewDeviceAuthClient(mock.URL())

	token, err := client.CheckTokenWithCSR("dev-code", testCSRPEM)
	if err != nil {
		t.Fatalf("CheckTokenWithCSR: %v", err)
	}
	if token.LeafPem != "LEAF" || token.ChainPem != "CHAIN" || token.NodeUID != "uid-123" {
		t.Errorf("bundle = (%q,%q,%q), want (LEAF,CHAIN,uid-123)", token.LeafPem, token.ChainPem, token.NodeUID)
	}
}

// TestPollForTokenWithCSR_DropsCSROnEnrollmentFailure proves the citadel-cli#1062
// availability fix: a certificate-enrollment failure on a CSR-bearing poll does
// NOT abort login. The CSR is dropped and polling continues, so the next no-CSR
// poll delivers the authkey (the node stays honestly unverified).
func TestPollForTokenWithCSR_DropsCSROnEnrollmentFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
	}{
		{"unavailable_503", http.StatusServiceUnavailable, "certificate_enrollment_unavailable"},
		{"rate_limited_429", http.StatusTooManyRequests, "certificate_enrollment_rate_limited"},
		{"conflict_409", http.StatusConflict, "certificate_enrollment_failed"},
		{"failed_400", http.StatusBadRequest, "certificate_enrollment_failed"},
		{"unsupported_kind_400", http.StatusBadRequest, "unsupported_device_kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := StartMockDeviceAuthServer(1)
			defer mock.Close()
			mock.FailCSREnrollment(tc.status, tc.code)
			client := NewDeviceAuthClient(mock.URL())

			token, err := client.PollForTokenWithCSR("dev-code", 0 /* fast */, testCSRPEM)
			if err != nil {
				t.Fatalf("PollForTokenWithCSR aborted on a CA-enrollment failure: %v", err)
			}
			if token.Authkey == "" {
				t.Fatal("expected an authkey after dropping the CSR")
			}

			csrs := mock.TokenRequestCSRs()
			if len(csrs) < 2 {
				t.Fatalf("expected at least 2 polls (CSR fails, no-CSR succeeds), got %d", len(csrs))
			}
			if csrs[0] != testCSRPEM {
				t.Errorf("first poll csr_pem = %q, want the submitted CSR", csrs[0])
			}
			if csrs[len(csrs)-1] != "" {
				t.Errorf("final (successful) poll csr_pem = %q, want none (CSR dropped)", csrs[len(csrs)-1])
			}
		})
	}
}

// TestPollForTokenWithCSR_TerminalErrorsStillAbort proves the drop-and-continue
// is scoped to the certificate-enrollment codes: a standard RFC 8628 terminal
// code, and any other unrecognized code, still abort rather than silently
// polling on without the CSR.
func TestPollForTokenWithCSR_TerminalErrorsStillAbort(t *testing.T) {
	for _, code := range []string{"access_denied", "expired_token", "some_future_code"} {
		t.Run(code, func(t *testing.T) {
			mock := StartMockDeviceAuthServer(1)
			defer mock.Close()
			// A 400 body carrying the code exercises the RFC 8628 parse path.
			mock.FailCSREnrollment(http.StatusBadRequest, code)
			client := NewDeviceAuthClient(mock.URL())

			token, err := client.PollForTokenWithCSR("dev-code", 0, testCSRPEM)
			if err == nil {
				t.Fatalf("expected abort on %q, got token %+v", code, token)
			}
		})
	}
}

// TestTokenResponse_NoBundleByteIdentical confirms a success response with no
// bundle omits the three fields (existing success-response consumers unchanged).
func TestTokenResponse_NoBundleByteIdentical(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()
	client := NewDeviceAuthClient(mock.URL())

	token, err := client.CheckToken("dev-code")
	if err != nil {
		t.Fatalf("CheckToken: %v", err)
	}
	if token.LeafPem != "" || token.ChainPem != "" || token.NodeUID != "" {
		t.Errorf("expected no bundle fields, got (%q,%q,%q)", token.LeafPem, token.ChainPem, token.NodeUID)
	}
}
