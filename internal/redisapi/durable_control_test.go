package redisapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type durableRoundTripper func(*http.Request) (*http.Response, error)

func (fn durableRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func controlFixture(t *testing.T) (*Client, *DeliveryClaim, string) {
	t.Helper()
	token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	claim, err := newDeliveryClaim(DeliveryIdentity{DispatchID: "c486d0e4-9cad-4334-b437-2d5d847a5279", RequestDigest: strings.Repeat("a", 64), Queue: "jobs:v1:org:fixture:node:123", MessageID: "123-0", LeaseEpoch: 7}, token)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(ClientConfig{BaseURL: "https://api.fixture.invalid", Token: "synthetic-device-key"})
	return client, claim, token
}

func renewalReply(identity DeliveryIdentity) map[string]any {
	return map[string]any{"dispatchId": identity.DispatchID, "requestDigest": identity.RequestDigest, "leaseEpoch": identity.LeaseEpoch, "expiresAt": "2001-01-01T00:00:00Z", "remainingLeaseMs": 1000}
}
func controlResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
}

func TestDurableRenewalUsesExactTupleAndRelativeBudget(t *testing.T) {
	client, claim, token := controlFixture(t)
	client.httpClient.Transport = durableRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/fabric/dispatches/"+claim.Identity().DispatchID+"/renew" || request.URL.RawQuery != "" {
			t.Fatal("incorrect renewal route")
		}
		if request.Header.Get("Authorization") != "Bearer synthetic-device-key" || request.Header.Get("x-claim-token") != token || request.Header.Get("x-dispatch-token") != "" || request.Header.Get("x-connection-token") != "" {
			t.Fatal("incorrect control credentials")
		}
		var body map[string]any
		if json.NewDecoder(request.Body).Decode(&body) != nil || len(body) != 4 || body["queue"] != claim.Identity().Queue || body["messageId"] != "123-0" || body["requestDigest"] != claim.Identity().RequestDigest || body["leaseEpoch"] != float64(7) {
			t.Fatal("renewal changed its original tuple")
		}
		raw, _ := json.Marshal(renewalReply(claim.Identity()))
		return controlResponse(string(raw)), nil
	})
	budget, err := client.RenewDelivery(context.Background(), claim)
	// The advisory timestamp is deliberately in the past. No local wall-clock
	// comparison is authority; the positive server budget is the only duration.
	if err != nil || budget != time.Second {
		t.Fatalf("relative budget lost: %v %v", budget, err)
	}
}

func TestDurableRenewalRejectsMalformedOrChangedReply(t *testing.T) {
	for _, name := range []string{"missing-budget", "zero", "negative", "fraction", "excess", "string", "null", "other-dispatch", "other-digest", "other-epoch", "unsafe-epoch", "bad-timestamp", "unknown-field", "duplicate", "trailing", "oversize"} {
		t.Run(name, func(t *testing.T) {
			client, claim, _ := controlFixture(t)
			reply := renewalReply(claim.Identity())
			switch name {
			case "missing-budget":
				delete(reply, "remainingLeaseMs")
			case "zero":
				reply["remainingLeaseMs"] = 0
			case "negative":
				reply["remainingLeaseMs"] = -1
			case "fraction":
				reply["remainingLeaseMs"] = 0.5
			case "excess":
				reply["remainingLeaseMs"] = 300001
			case "string":
				reply["remainingLeaseMs"] = "1000"
			case "null":
				reply["remainingLeaseMs"] = nil
			case "other-dispatch":
				reply["dispatchId"] = "4db83075-fc42-483f-a0ad-b087b12b76f4"
			case "other-digest":
				reply["requestDigest"] = strings.Repeat("b", 64)
			case "other-epoch":
				reply["leaseEpoch"] = 8
			case "unsafe-epoch":
				reply["leaseEpoch"] = uint64(1 << 53)
			case "bad-timestamp":
				reply["expiresAt"] = "2001-01-01"
			case "unknown-field":
				reply["extra"] = true
			}
			raw, _ := json.Marshal(reply)
			body := string(raw)
			if name == "duplicate" {
				body = strings.TrimSuffix(body, "}") + `,"remainingLeaseMs":1000}`
			}
			if name == "trailing" {
				body += " null"
			}
			if name == "oversize" {
				body += strings.Repeat(" ", 4097)
			}
			client.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) { return controlResponse(body), nil })
			if budget, err := client.RenewDelivery(context.Background(), claim); err == nil || budget != 0 {
				t.Fatalf("invalid renewal admitted: %s %v %v", name, budget, err)
			}
		})
	}
}

func TestDurableControlRefusesReplyWithoutDisclosingCredentials(t *testing.T) {
	client, claim, token := controlFixture(t)
	for _, code := range []int{400, 401, 403, 409, 429, 500} {
		client.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(token + " synthetic-device-key"))}, nil
		})
		if _, err := client.RenewDelivery(context.Background(), claim); err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "synthetic-device-key") {
			t.Fatal("control refusal leaked credentials or succeeded")
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%#x", "%q", "%d"} {
		if formatted := fmt.Sprintf(verb, claim); strings.Contains(formatted, token) || strings.Contains(formatted, claim.Identity().DispatchID) {
			t.Fatal("claim formatting exposed its capture")
		}
	}
	data, err := json.Marshal(claim)
	if err != nil || string(data) != "null" {
		t.Fatal("claim entered JSON output")
	}
}

func TestDurableControlIntegerWireCompatibility(t *testing.T) {
	for _, raw := range []string{"1000", "1000.0", "1e3", "0.01e5"} {
		var value uint64
		if !controlInteger([]byte(raw), 300000, &value) || value != 1000 {
			t.Fatalf("valid integer spelling refused: %s", raw)
		}
	}
	for _, raw := range []string{`"1000"`, "null", "true", "1.5", "9007199254740991.1", "1e999999999999", "1e-999999999999"} {
		var value uint64
		if controlInteger([]byte(raw), maxDurableEpoch, &value) {
			t.Fatalf("invalid integer accepted: %s", raw)
		}
	}
}

func TestDurableCompletionValidatesReceiptWithoutReadingResults(t *testing.T) {
	for _, state := range []string{"committed", "replayed", "ack_repaired"} {
		client, claim, token := controlFixture(t)
		client.httpClient.Transport = durableRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/api/fabric/dispatches/"+claim.Identity().DispatchID+"/complete" || request.URL.Query().Get("queue") != claim.Identity().Queue || request.URL.Query().Get("messageId") != "123-0" || request.URL.Query().Get("leaseEpoch") != "7" || request.Header.Get("x-claim-token") != token {
				t.Fatal("completion tuple/credential changed")
			}
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"result":{"content":"fixture"}}` {
				t.Fatal("terminal envelope changed")
			}
			receipt := map[string]any{"dispatchId": claim.Identity().DispatchID, "status": state, "envelopeSha256": strings.Repeat("b", 64), "committedAt": "2026-10-01T00:00:00.000001Z", "payloadExpiresAt": "2026-10-02T00:00:00.000001Z", "outboxId": "123-0"}
			raw, _ := json.Marshal(receipt)
			return controlResponse(string(raw)), nil
		})
		if err := client.CompleteDelivery(context.Background(), claim, []byte(`{"result":{"content":"fixture"}}`)); err != nil {
			t.Fatalf("valid completion receipt refused: %s %v", state, err)
		}
	}
}

func TestDurableCompletionRefusesInvalidReceipt(t *testing.T) {
	for _, name := range []string{"dispatch", "state", "digest", "outbox", "timestamp", "precision", "expiry-order", "unknown", "duplicate", "oversize", "trailing", "null"} {
		t.Run(name, func(t *testing.T) {
			client, claim, _ := controlFixture(t)
			reply := map[string]any{"dispatchId": claim.Identity().DispatchID, "status": "committed", "envelopeSha256": strings.Repeat("b", 64), "committedAt": "2026-10-28T00:00:00.000001Z", "payloadExpiresAt": "2026-10-29T00:00:00.000001Z", "outboxId": "123-0"}
			switch name {
			case "dispatch":
				reply["dispatchId"] = "4db83075-fc42-483f-a0ad-b087b12b76f4"
			case "state":
				reply["status"] = "accepted"
			case "digest":
				reply["envelopeSha256"] = "not-a-digest"
			case "outbox":
				reply["outboxId"] = "001-0"
			case "timestamp":
				reply["committedAt"] = "yesterday"
			case "precision":
				reply["committedAt"] = "2026-10-28T00:00:00.1Z"
			case "expiry-order":
				reply["payloadExpiresAt"] = reply["committedAt"]
			case "unknown":
				reply["result"] = map[string]any{"private": "must never read"}
			}
			raw, _ := json.Marshal(reply)
			body := string(raw)
			switch name {
			case "duplicate":
				body = strings.TrimSuffix(body, "}") + `,"status":"committed"}`
			case "oversize":
				body += strings.Repeat(" ", 4097)
			case "trailing":
				body += " null"
			case "null":
				body = "null"
			}
			client.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) { return controlResponse(body), nil })
			if err := client.CompleteDelivery(context.Background(), claim, []byte(`{"result":null}`)); err == nil {
				t.Fatal("invalid commit receipt admitted", name)
			}
		})
	}
}
func TestDurableControlNeverFollowsCredentialRedirect(t *testing.T) {
	client, claim, _ := controlFixture(t)
	calls := 0
	client.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			raw, _ := json.Marshal(renewalReply(claim.Identity()))
			return controlResponse(string(raw)), nil
		}
		r := controlResponse(`{}`)
		r.StatusCode = 307
		r.Header = http.Header{"Location": []string{"https://redirect.fixture.invalid/leak"}}
		return r, nil
	})
	if _, err := client.RenewDelivery(context.Background(), claim); err == nil || calls != 1 {
		t.Fatal("claim followed redirect", err, calls)
	}
}

func TestDurableRenewalPreservesDifferentRelativeBudgets(t *testing.T) {
	for _, ms := range []int{1, 80, 1739, 300000} {
		client, claim, _ := controlFixture(t)
		reply := renewalReply(claim.Identity())
		reply["remainingLeaseMs"] = ms
		raw, _ := json.Marshal(reply)
		client.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) { return controlResponse(string(raw)), nil })
		budget, err := client.RenewDelivery(context.Background(), claim)
		if err != nil || budget != time.Duration(ms)*time.Millisecond {
			t.Fatal("server relative budget changed", ms, budget, err)
		}
	}
}
