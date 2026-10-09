package heartbeat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func sampleAutoUpdateReport() *status.AutoUpdateReport {
	cfg, eff := true, false
	ts := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	return &status.AutoUpdateReport{
		SchemaVersion:     1,
		ConfiguredEnabled: &cfg,
		EffectiveEnabled:  &eff,
		PolicySource:      "env",
		Mode:              "periodic",
		IntervalSeconds:   3600,
		RunningVersion:    "v2.0.0",
		PendingVersion:    "v2.1.0",
		RestartRequired:   true,
		LastCheckAt:       &ts,
		LatestVersion:     "v2.1.0",
		LastResultAt:      &ts,
		LastResult:        "up_to_date",
	}
}

// TestHeartbeatAutoUpdateOmittedWhenNoProvider pins the byte-unchanged
// guarantee (citadel-cli#1134): a process that wires no auto-update provider
// emits a heartbeat with no auto_update key at all.
func TestHeartbeatAutoUpdateOmittedWhenNoProvider(t *testing.T) {
	pub, mr, _ := newTestRedisPublisher(t)
	if err := pub.publishStatus(context.Background()); err != nil {
		t.Fatalf("publishStatus: %v", err)
	}
	entries := streamEntries(t, mr)
	if len(entries) != 1 {
		t.Fatalf("want 1 stream entry, got %d", len(entries))
	}
	if strings.Contains(entries[0]["payload"], "auto_update") {
		t.Errorf("heartbeat must omit auto_update when no provider is set; payload=%s", entries[0]["payload"])
	}
}

// TestBothTransportsEmitEquivalentAutoUpdateReport pins that the Redis and API
// transports emit the identical v1 auto_update block from the same provider
// (citadel-cli#1134 acceptance), proving neither envelope drops or reshapes it.
func TestBothTransportsEmitEquivalentAutoUpdateReport(t *testing.T) {
	report := sampleAutoUpdateReport()
	provider := func() *status.AutoUpdateReport { return report }

	// Redis transport: read the payload back out of the durable stream.
	rpub, mr, _ := newTestRedisPublisher(t)
	rpub.SetAutoUpdateProvider(provider)
	if err := rpub.publishStatus(context.Background()); err != nil {
		t.Fatalf("redis publishStatus: %v", err)
	}
	entries := streamEntries(t, mr)
	if len(entries) != 1 {
		t.Fatalf("want 1 redis stream entry, got %d", len(entries))
	}
	var redisMsg StatusMessage
	if err := json.Unmarshal([]byte(entries[0]["payload"]), &redisMsg); err != nil {
		t.Fatalf("unmarshal redis payload: %v", err)
	}

	// API transport: the stub decodes the stream payload for us.
	stub := &redisAPIStub{}
	apub, _ := newTestAPIPublisher(t, stub)
	apub.SetAutoUpdateProvider(provider)
	if err := apub.publishStatus(context.Background()); err != nil {
		t.Fatalf("api publishStatus: %v", err)
	}
	landed := stub.landed()
	if len(landed) != 1 {
		t.Fatalf("want 1 api stream payload, got %d", len(landed))
	}
	apiMsg := landed[0]

	if redisMsg.AutoUpdate == nil || apiMsg.AutoUpdate == nil {
		t.Fatalf("both transports must carry auto_update (redis=%v api=%v)", redisMsg.AutoUpdate, apiMsg.AutoUpdate)
	}
	rJSON, _ := json.Marshal(redisMsg.AutoUpdate)
	aJSON, _ := json.Marshal(apiMsg.AutoUpdate)
	if string(rJSON) != string(aJSON) {
		t.Errorf("transports emit divergent auto_update JSON:\n redis=%s\n api  =%s", rJSON, aJSON)
	}

	// Spot-check the v1 contract survived the round trip.
	if redisMsg.AutoUpdate.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", redisMsg.AutoUpdate.SchemaVersion)
	}
	if redisMsg.AutoUpdate.LastResult != "up_to_date" {
		t.Errorf("last_result = %q, want up_to_date", redisMsg.AutoUpdate.LastResult)
	}
	if redisMsg.AutoUpdate.PolicySource != "env" {
		t.Errorf("policy_source = %q, want env", redisMsg.AutoUpdate.PolicySource)
	}
}
