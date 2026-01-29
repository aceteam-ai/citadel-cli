// internal/jobs/handler.go
package jobs

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// JobContext can hold shared resources like a logger, config, etc.
type JobContext struct {
	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)
}

// Log outputs a message - uses LogFn callback if set, otherwise prints to stdout.
func (c *JobContext) Log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.LogFn != nil {
		c.LogFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
}

// JobHandler is the interface that all job executors must implement.
type JobHandler interface {
	Execute(ctx JobContext, job *nexus.Job) (output []byte, err error)
}
