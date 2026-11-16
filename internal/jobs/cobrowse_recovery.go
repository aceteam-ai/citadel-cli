package jobs

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/cobrowserecovery"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

type recoveryBrowserOps interface {
	cobrowserecovery.Ops
	Status(string) (platform.CobrowseSessionStatus, bool)
}

func hasRecoveryScope(job *nexus.Job) bool {
	for _, key := range []string{"start_job_id", "caller_org_id", "owner_user_id"} {
		if _, ok := job.Payload[key]; ok {
			return true
		}
	}
	return false
}
func (h *CobrowseSessionHandler) recoverJob(action string, job *nexus.Job) ([]byte, error) {
	p := job.Payload
	id := p["start_job_id"]
	if action == "start" {
		id = job.ID
		if p["start_job_id"] != "" && p["start_job_id"] != id {
			return nil, cobrowserecovery.ErrConflict
		}
	}
	// SourceQueue is copied from transport custody, not caller payload. Require
	// exactly one per-node segment, the exact local control identity and a nonempty
	// routing org. Shared-node caller_org_id deliberately need not equal node org.
	prefix := "jobs:v1:shell:org_"
	tail := strings.TrimPrefix(job.SourceQueue, prefix)
	parts := strings.Split(tail, ":node:")
	if h.recoveryNodeID == "" || !strings.HasPrefix(job.SourceQueue, prefix) || len(parts) != 2 || parts[0] == "" || parts[1] != h.recoveryNodeID || strings.ContainsAny(parts[0], ":/\\ \t\r\n") {
		return nil, cobrowserecovery.ErrConflict
	}
	scope := cobrowserecovery.Scope{JobID: id, Node: h.recoveryNodeID, Org: p["caller_org_id"], Owner: p["owner_user_id"]}
	digest := ""
	if action == "start" {
		var err error
		digest, err = cobrowserecovery.Digest(p["url"], p["profile"])
		if err != nil {
			return nil, err
		}
	}
	if h.recoveryRoot == nil {
		return nil, cobrowserecovery.ErrUnavailable
	}
	root := h.recoveryRoot()
	if root == "" {
		return nil, cobrowserecovery.ErrUnavailable
	}
	var ops recoveryBrowserOps
	if h.recoveryOps != nil {
		ops = h.recoveryOps(job)
	} else {
		ops = &nodeRecoveryBrowser{manager: platform.GetCobrowseSessionManager(), job: job, root: root}
	}
	result, err := (cobrowserecovery.Engine{Store: cobrowserecovery.FileStore{Dir: filepath.Join(root, "cobrowse-job-recovery")}}).Do(action, scope, digest, ops)
	if err != nil {
		return nil, err
	}
	if action == "start" && result.State == cobrowserecovery.Running {
		if st, ok := ops.Status(result.SessionID); ok {
			b, err := sessionStatusResult(st)
			if err != nil {
				return nil, err
			}
			var out map[string]any
			if err = json.Unmarshal(b, &out); err != nil {
				return nil, err
			}
			out["start_job_id"] = result.JobID
			return json.Marshal(out)
		}
	}
	return json.Marshal(result)
}

type nodeRecoveryBrowser struct {
	manager *platform.CobrowseSessionManager
	job     *nexus.Job
	root    string
}

func (b *nodeRecoveryBrowser) Start(id string) (bool, error) {
	p := b.job.Payload
	if err := platform.ValidateCobrowseURL(p["url"]); err != nil {
		return false, cobrowserecovery.ErrConflict
	}
	var handle *cobrowseprofile.Handle
	if p["profile"] != "" {
		if p["pin"] == "" {
			return false, cobrowserecovery.ErrConflict
		}
		var err error
		handle, err = cobrowseprofile.OpenHandle(b.root, p["profile"], p["pin"], nodevault.Open(b.root))
		if err != nil {
			return false, cobrowserecovery.ErrConflict
		}
	}
	// A typed nil profile must not become a non-nil interface.
	var profile platform.SessionProfile
	if handle != nil {
		profile = handle
	}
	_, err := b.manager.StartRecoverySessionWithID(id, p["url"], profile)
	return true, err
}
func (b *nodeRecoveryBrowser) Observe(id string) cobrowserecovery.State {
	return cobrowserecovery.State(b.manager.RecoveryState(id))
}
func (b *nodeRecoveryBrowser) Stop(id string) (bool, error) { return b.manager.StopRecoverySession(id) }
func (b *nodeRecoveryBrowser) Status(id string) (platform.CobrowseSessionStatus, bool) {
	return b.manager.SessionStatus(id)
}

// CobrowseJobRecoveryAvailable reports protocol support, not permission to start
// a browser. Every request still requires the private journal and scoped checks.
func (h *CobrowseSessionHandler) CobrowseJobRecoveryAvailable() bool {
	return cobrowserecovery.Supported() && h.recoveryNodeID != ""
}
