package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/resmon"
)

// ResourceSnapshotHandler backs the RESOURCE_SNAPSHOT job type (issue #427): it
// returns the node's full resource-consumer snapshot — every GPU compute
// process, managed or not, with attributed VRAM/RSS and a reclaimable flag — so
// the fabric can pull the picture over the Redis mesh without SSH. It is the
// job-transport twin of the status server's GET /resources, delegating to the
// same resmon.Collect so both surfaces stay identical.
type ResourceSnapshotHandler struct{}

// Execute collects a resource snapshot and returns it as JSON. It takes no
// payload fields. The snapshot never errors internally (a missing GPU / runtime
// degrades to "unknown"), so this handler only fails if JSON marshaling does.
func (h *ResourceSnapshotHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	collectCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	snapshot := resmon.Collect(collectCtx)

	out, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal resource snapshot: %w", err)
	}
	ctx.Log("info", "     - [Job %s] RESOURCE_SNAPSHOT: %d consumer(s), gpu %d/%d bytes",
		job.ID, len(snapshot.Consumers), snapshot.GPU.UsedBytes, snapshot.GPU.TotalBytes)
	return out, nil
}

// Ensure ResourceSnapshotHandler implements JobHandler.
var _ JobHandler = (*ResourceSnapshotHandler)(nil)
