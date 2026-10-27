package cmd

import (
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestWorkerHeartbeatPreservesActualPlatformEvidence(t *testing.T) {
	s := worker.NewWorkerState()
	ok := time.Unix(1700000000, 0)
	failed := ok.Add(time.Minute)
	s.ObservePlatform(platformhttp.Observation{At: ok, Outcome: platformhttp.HTTPHealthy, Status: 204})
	s.ObservePlatform(platformhttp.Observation{At: failed, Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
	live := workerLivenessFrom(s.Snapshot())
	if live.PlatformOKAt == nil || !live.PlatformOKAt.Equal(ok) || live.PlatformFailingSince == nil || !live.PlatformFailingSince.Equal(failed) || live.PlatformLastOutcome != "wrong_answer" || live.PlatformLastHTTPStatus != 503 {
		t.Fatal("heartbeat projection lost platform HTTP provenance")
	}
}
