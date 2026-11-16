package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowserecovery"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const recoveryJobID = "c8409b9a-fcfb-4677-b9f2-813bc0110b91"
const recoveryOrg = "4b740eac-734f-4427-a019-32cf52dc3d99"
const recoveryOwner = "76c03b14-c760-4b01-9c92-c8597350fcbb"

type fixtureRecoveryBrowser struct {
	starts, stops, statusCalls int
	state                      cobrowserecovery.State
	id                         string
}

func (f *fixtureRecoveryBrowser) Start(id string) (bool, error) {
	f.starts++
	f.id = id
	f.state = cobrowserecovery.Running
	return true, nil
}
func (f *fixtureRecoveryBrowser) Observe(string) cobrowserecovery.State { return f.state }
func (f *fixtureRecoveryBrowser) Stop(string) (bool, error) {
	f.stops++
	f.state = cobrowserecovery.Ended
	return true, nil
}
func (f *fixtureRecoveryBrowser) Status(id string) (platform.CobrowseSessionStatus, bool) {
	f.statusCalls++
	return platform.CobrowseSessionStatus{ID: id, URL: "https://example.test/?secret=page-query", State: platform.SessionRunning}, true
}
func recoveryFixture(t *testing.T) (*CobrowseSessionHandler, *nexus.Job, *fixtureRecoveryBrowser) {
	t.Helper()
	if !cobrowserecovery.Supported() {
		t.Skip("Unix adapter")
	}
	h := NewCobrowseSessionHandlerForNode("1297")
	root := t.TempDir()
	h.recoveryRoot = func() string { return root }
	f := &fixtureRecoveryBrowser{}
	h.recoveryOps = func(*nexus.Job) recoveryBrowserOps { return f }
	job := &nexus.Job{ID: recoveryJobID, Type: "COBROWSE_SESSION", SourceQueue: "jobs:v1:shell:org_node-owner:node:1297", Payload: map[string]string{"action": "start", "caller_org_id": recoveryOrg, "owner_user_id": recoveryOwner, "url": "https://example.test", "pin": "PIN-secret"}}
	return h, job, f
}
func TestCobrowseRecoveryHandlerLostReplyAndScopedReceipt(t *testing.T) {
	h, j, f := recoveryFixture(t)
	b, e := h.Execute(JobContext{}, j)
	if e != nil {
		t.Fatal(e)
	}
	var started map[string]any
	json.Unmarshal(b, &started)
	if started["start_job_id"] != recoveryJobID || started["id"] != f.id || started["stream"] == nil {
		t.Fatalf("legacy start fields lost: %s", b)
	}
	j.Payload["start_job_id"] = recoveryJobID
	j.Payload["action"] = "job_status"
	b, e = h.Execute(JobContext{}, j)
	if e != nil {
		t.Fatal(e)
	}
	var result cobrowserecovery.Result
	json.Unmarshal(b, &result)
	if result.State != cobrowserecovery.Running || result.SessionID != f.id || f.statusCalls != 1 {
		t.Fatalf("unsafe recovery receipt: %s", b)
	}
	if strings.Contains(string(b), "query") || strings.Contains(string(b), "PIN") || strings.Contains(string(b), "url") {
		t.Fatal("receipt exposed page or credential")
	}
	j.Payload["action"] = "stop_by_job_id"
	b, e = h.Execute(JobContext{}, j)
	if e != nil || !strings.Contains(string(b), `"state":"stopped"`) || f.stops != 1 {
		t.Fatalf("quota stop failed %s %v", b, e)
	}
	j.Payload["action"] = "start"
	if _, e = h.Execute(JobContext{}, j); e != nil || f.starts != 1 {
		t.Fatal("terminal replay launched")
	}
}
func TestCobrowseRecoveryHandlerRejectsRoutingAndIncompleteScope(t *testing.T) {
	for _, kind := range []string{"pool", "tag", "wrong-target", "wrong-stream-node", "empty-node", "payload-forged-queue", "missing-org", "missing-owner", "bad-start-id"} {
		t.Run(kind, func(t *testing.T) {
			h, j, f := recoveryFixture(t)
			switch kind {
			case "pool":
				j.SourceQueue = "jobs:v1:shell:org_node-owner"
			case "tag":
				j.SourceQueue = "jobs:v1:shell:org_node-owner:tag:node:1297"
			case "wrong-target":
				h.recoveryNodeID = "9"
			case "wrong-stream-node":
				j.SourceQueue = "jobs:v1:shell:org_node-owner:node:8"
			case "empty-node":
				h.recoveryNodeID = ""
			case "payload-forged-queue":
				j.Payload["SourceQueue"] = j.SourceQueue
				j.SourceQueue = ""
			case "missing-org":
				delete(j.Payload, "caller_org_id")
			case "missing-owner":
				delete(j.Payload, "owner_user_id")
			case "bad-start-id":
				j.Payload["start_job_id"] = "another"
			}
			if _, e := h.Execute(JobContext{}, j); !errors.Is(e, cobrowserecovery.ErrConflict) || f.starts != 0 {
				t.Fatalf("scope accepted: %v starts=%d", e, f.starts)
			}
		})
	}
}
func TestCobrowseRecoveryHandlerForeignOrgAndOwnerDenied(t *testing.T) {
	h, j, f := recoveryFixture(t)
	if _, e := h.Execute(JobContext{}, j); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"caller_org_id", "owner_user_id"} {
		t.Run(key, func(t *testing.T) {
			original := j.Payload[key]
			defer func() { j.Payload[key] = original }()
			j.Payload[key] = "f210409c-7297-4a20-b072-2d20c97830a0"
			j.Payload["start_job_id"] = recoveryJobID
			for _, action := range []string{"job_status", "stop_by_job_id"} {
				j.Payload["action"] = action
				b, e := h.Execute(JobContext{}, j)
				if !errors.Is(e, cobrowserecovery.ErrConflict) || len(b) != 0 {
					t.Fatalf("foreign receipt leaked: %s %v", b, e)
				}
			}
		})
	}
	if f.starts != 1 || f.stops != 0 {
		t.Fatal("foreign request touched browser")
	}
}
func TestCobrowseRecoveryHandlerStopBeforeStartAndNotFound(t *testing.T) {
	h, j, f := recoveryFixture(t)
	j.Payload["action"] = "job_status"
	j.Payload["start_job_id"] = recoveryJobID
	b, e := h.Execute(JobContext{}, j)
	if e != nil || !strings.Contains(string(b), `"state":"not_found"`) {
		t.Fatalf("%s %v", b, e)
	}
	j.Payload["action"] = "stop_by_job_id"
	if _, e = h.Execute(JobContext{}, j); e != nil {
		t.Fatal(e)
	}
	j.Payload["action"] = "start"
	b, e = h.Execute(JobContext{}, j)
	if e != nil || !strings.Contains(string(b), `"state":"stopped"`) || f.starts != 0 {
		t.Fatalf("late start: %s %v", b, e)
	}
}
