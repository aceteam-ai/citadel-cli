package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMediaRoutingDiagnosticRetentionBoundary(t *testing.T) {
	const secret = "synthetic-routing-input.secret"
	for _, kind := range []string{"MEDIA_GENERATE", "FILE_FETCH", "SHELL_COMMAND"} {
		for _, node := range []string{"", "different-node", secret} {
			for _, private := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/node=%q/private=%v", kind, node, private), func(t *testing.T) {
					payload := map[string]any{"target_node": secret}
					if private {
						switch kind {
						case "MEDIA_GENERATE":
							payload["inputs"] = map[string]any{"capability": secret}
						case "FILE_FETCH":
							payload["capability"] = secret
						case "SHELL_COMMAND":
							payload["passcode"] = secret
						}
					}
					raw, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					job := &Job{ID: "fixture-job", Type: kind, Payload: payload, RawPayload: raw}
					var logs []string
					source := NewMockJobSource("fixture", nil)
					runner := NewRunner(source, nil, RunnerConfig{NodeID: node, ActivityFn: func(_ string, message string) {
						logs = append(logs, message)
					}})
					writers := 0
					writer := &MockStreamWriter{}
					runner.WithStreamWriterFactory(func(*Job) StreamWriter {
						writers++
						return writer
					})
					proceed, stream, _ := runner.claimJob(context.Background(), job)
					matches := node == secret
					if proceed != matches || len(source.AckedJobs()) != map[bool]int{false: 1, true: 0}[matches] || writers != map[bool]int{false: 0, true: 1}[matches] {
						t.Fatal("diagnostic policy changed target matching, skip ACK or claim publication")
					}
					if matches != (stream != nil) || len(source.FailedJobs()) != 0 || len(source.NackedJobs()) != 0 {
						t.Fatal("diagnostic policy changed delivery disposition")
					}
					if private && strings.Contains(strings.Join(logs, "\n"), secret) {
						t.Fatal("payload-derived routing diagnostic retained private input detail")
					}
					if !private && !matches && !strings.Contains(strings.Join(logs, "\n"), secret) {
						t.Fatal("ordinary routing diagnostic unexpectedly changed")
					}
					projection, err := job.executionPayload()
					original, originalOK := rawOrPayload(job).(string)
					if err != nil || projection["target_node"] != secret || !originalOK || !bytes.Equal([]byte(original), raw) {
						t.Fatal("diagnostic omission changed original routing or receipt bytes")
					}
				})
			}
		}
	}
}
