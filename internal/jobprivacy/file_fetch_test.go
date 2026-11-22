package jobprivacy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFileFetchErrorRedactionPreservesClassification(t *testing.T) {
	const token = "synthetic-delivery.synthetic-credential"
	payload := map[string]any{"capability": token}
	original := fmt.Errorf("%w: %s", context.DeadlineExceeded, token)
	safe := Error(FileFetch, payload, original)
	if strings.Contains(safe.Error(), token) || !errors.Is(safe, context.DeadlineExceeded) {
		t.Fatal("redaction lost error classification or retained the credential")
	}
	if Error("OTHER", payload, original) != original || Payload("OTHER", payload)["capability"] != token {
		t.Fatal("unrelated job semantics changed")
	}
	if Payload(FileFetch, payload) != nil || payload["capability"] != token {
		t.Fatal("retention did not omit payload or mutated execution authority")
	}
}

func TestAMTCapabilityIsAbsentFromRetainedDiagnostics(t *testing.T) {
	const token = "synthetic-amt-capability"
	const endpoint = "https://example.test/api/fabric/amt/credential"
	payload := map[string]any{"credential_token": token, "credential_url": endpoint, "host": "192.168.1.20"}
	if Payload(AMTKVM, payload) != nil {
		t.Fatal("AMT payload must be omitted from retained diagnostics")
	}
	original := fmt.Errorf("%w: %s %s", context.DeadlineExceeded, token, endpoint)
	safe := Error(AMTKVM, payload, original)
	if !errors.Is(safe, context.DeadlineExceeded) || strings.Contains(safe.Error(), token) || strings.Contains(safe.Error(), endpoint) {
		t.Fatal("AMT error retained a capability or lost classification")
	}
	fields := FailureData(AMTKVM, payload, original, map[string]any{"credential_token": token})
	if len(fields) != 1 || strings.Contains(fmt.Sprint(fields), token) || strings.Contains(fmt.Sprint(fields), endpoint) {
		t.Fatal("AMT failure data retained a capability")
	}
	if payload["credential_token"] != token {
		t.Fatal("redaction mutated execution authority")
	}
}
