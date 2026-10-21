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
