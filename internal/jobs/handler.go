// internal/jobs/handler.go
package jobs

import "github.com/aceboss/citadel-cli/internal/nexus"

// JobContext can hold shared resources like a logger, config, etc.
type JobContext struct {
	// For now, it's empty, but we can add things later.
}

// JobHandler is the interface that all job executors must implement.
type JobHandler interface {
	Execute(ctx JobContext, job *nexus.Job) (output []byte, err error)
}
