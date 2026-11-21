package redisapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func durableFixtureWire(t *testing.T) string {
	t.Helper()
	payload := `{ "value":9007199254740993,"text":"雪" }`
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf(`{"messages":[{"id":"123-0","data":{"jobId":%q,"type":"SHELL_COMMAND","payload":%q,"protocol":%q,"dispatchId":%q,"requestDigest":%q,"claimToken":%q,"leaseEpoch":"7"}}]}`, "c486d0e4-9cad-4334-b437-2d5d847a5279", payload, DurableResultsProtocol, "c486d0e4-9cad-4334-b437-2d5d847a5279", hex.EncodeToString(sum[:]), strings.Repeat("eHh4", 10)+"eHg")
}
func TestDurableConsumeNegotiatesOnlyExactMismatchAndCachesPerQueue(t *testing.T) {
	c, _, _ := controlFixture(t)
	var requests []ConsumeRequest
	c.httpClient.Transport = durableRoundTripper(func(r *http.Request) (*http.Response, error) {
		var req ConsumeRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Fatal("request not JSON")
		}
		requests = append(requests, req)
		if req.Queue == "jobs:v1:tag:durable.fixture" && req.Protocol == "" {
			res := controlResponse(`{"error":"Queue protocol mismatch"}`)
			res.StatusCode = 409
			return res, nil
		}
		if req.Protocol == DurableResultsProtocol {
			if req.Group != DurableConsumerGroup {
				t.Fatal("v1 used legacy group")
			}
			return controlResponse(durableFixtureWire(t)), nil
		}
		if req.Group != "per-node-123" {
			t.Fatal("legacy group changed")
		}
		return controlResponse(`{"messages":[]}`), nil
	})
	req := ConsumeRequest{Queue: "jobs:v1:tag:durable.fixture", Group: "per-node-123", Count: 1, BlockMs: 1}
	job, err := c.ConsumeJob(context.Background(), req)
	if err != nil || job == nil || job.ClaimedDelivery() == nil || !job.HasDurableMetadata() {
		t.Fatalf("HTTP claim not adopted: %v", err)
	}
	if len(requests) != 2 || requests[0].Protocol != "" || requests[1].Protocol != DurableResultsProtocol {
		t.Fatal("protocol was not negotiated once")
	}
	if string(job.RawPayload) != `{ "value":9007199254740993,"text":"雪" }` {
		t.Fatal("digest payload normalized")
	}
	if _, err = c.ConsumeJob(context.Background(), req); err != nil || len(requests) != 3 || requests[2].Protocol != DurableResultsProtocol {
		t.Fatal("successful queue protocol not cached", err)
	}
	req.Queue = "jobs:v1:cpu-general"
	if _, err = c.ConsumeJob(context.Background(), req); err != nil || len(requests) != 4 || requests[3].Protocol != "" {
		t.Fatal("protocol leaked across queues", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%#x", "%q", "%d"} {
		if strings.Contains(fmt.Sprintf(verb, job), strings.Repeat("eHh4", 10)+"eHg") {
			t.Fatal("Job formatting leaked token")
		}
	}
}
func TestDurableConsumeDoesNotNegotiateOtherRefusals(t *testing.T) {
	for _, body := range []string{`{"error":"other conflict"}`, `{"error":"Queue protocol mismatch","extra":1}`, `{"error":"Queue protocol mismatch","error":"other"}`, `not JSON`} {
		c, _, _ := controlFixture(t)
		calls := 0
		c.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			r := controlResponse(body)
			r.StatusCode = 409
			return r, nil
		})
		if _, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:cpu-general", Group: "legacy"}); err == nil || calls != 1 {
			t.Fatal("generic conflict triggered protocol fallback", err, calls)
		}
	}
}
func TestDurableConsumeRegistryRemovalReturnsToOriginalGroup(t *testing.T) {
	c, _, _ := controlFixture(t)
	c.consumeProtocols.Store("jobs:v1:cpu-general", true)
	calls := 0
	c.httpClient.Transport = durableRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		var req ConsumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if calls == 1 {
			if req.Protocol != DurableResultsProtocol || req.Group != DurableConsumerGroup {
				t.Fatal("cached v1 missing")
			}
			res := controlResponse(`{"error":"Queue protocol mismatch"}`)
			res.StatusCode = 409
			return res, nil
		}
		if req.Protocol != "" || req.Group != "node-original" {
			t.Fatal("legacy retry did not restore original group")
		}
		return controlResponse(`{"messages":[]}`), nil
	})
	if _, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:cpu-general", Group: "node-original"}); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}
func TestDurableClaimRefusesChangedOriginalAndRepeatedScopes(t *testing.T) {
	wire := durableFixtureWire(t)
	for name, raw := range map[string]string{
		"digest":    strings.Replace(wire, `"value\":9007199254740993`, `"value\":9007199254740992`, 1),
		"job-id":    strings.Replace(wire, `"jobId":"c486d0e4`, `"jobId":"d486d0e4`, 1),
		"epoch":     strings.Replace(wire, `"leaseEpoch":"7"`, `"leaseEpoch":"07"`, 1),
		"token":     strings.Replace(wire, `"claimToken":"`, `"claimToken":"!`, 1),
		"type":      strings.Replace(wire, `"type":"SHELL_COMMAND"`, `"type":""`, 1),
		"duplicate": strings.Replace(wire, `"leaseEpoch":"7"`, `"leaseEpoch":"7","leaseEpoch":"7"`, 1),
		"casing":    strings.Replace(wire, `"protocol":`, `"Protocol":`, 1),
		"container": strings.Replace(wire, `"data":{`, `"data":{},"data":{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if raw == wire {
				t.Fatal("fixture mutation did not apply")
			}
			c, _, _ := controlFixture(t)
			c.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) { return controlResponse(raw), nil })
			if job, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:cpu-general", Group: DurableConsumerGroup, Protocol: DurableResultsProtocol}); err == nil || job != nil {
				t.Fatal("invalid original became execution candidate")
			}
		})
	}
}
func TestDurableGenericParserAndLegacyResponseCannotMintClaim(t *testing.T) {
	raw := durableFixtureWire(t)
	var response ConsumeResponse
	if json.Unmarshal([]byte(raw), &response) != nil {
		t.Fatal("fixture")
	}
	parsed, err := ParseStreamMessage(response.Messages[0])
	if err != nil || parsed.ClaimedDelivery() != nil {
		t.Fatal("generic parse minted authority")
	}
	c, _, _ := controlFixture(t)
	c.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) { return controlResponse(raw), nil })
	job, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:cpu-general", Group: "legacy"})
	if err != nil || job == nil || !job.HasDurableMetadata() || job.ClaimedDelivery() != nil {
		t.Fatal("legacy consume minted v1 authority", err)
	}
}

func TestDurableRepeatedResponseCannotEraseRefusalMarker(t *testing.T) {
	original := durableFixtureWire(t)
	raw := strings.TrimSuffix(original, "}") + `,"messages":null,"messages":[{"id":"123-0"}]}`
	client, _, _ := controlFixture(t)
	client.httpClient.Transport = durableRoundTripper(func(*http.Request) (*http.Response, error) { return controlResponse(raw), nil })
	job, err := client.ConsumeJob(context.Background(), ConsumeRequest{Queue: "jobs:v1:cpu-general", Group: "legacy"})
	if err != nil || job == nil || !job.HasDurableMetadata() || job.ClaimedDelivery() != nil {
		t.Fatal("a repeated container erased the v1 refusal marker", err)
	}
}
