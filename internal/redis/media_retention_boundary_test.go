package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

const mediaBoundaryToken = "synthetic-media-boundary.secret"

type mediaDLQFixture struct {
	name    string
	payload any
	private bool
}

func mediaDLQFixtures() []mediaDLQFixture {
	return []mediaDLQFixture{
		{"inbox", `{"inputs":{"capability":"` + mediaBoundaryToken + `"}}`, true},
		{"workspace", `{"inputs":[{"origin":"workspace","path":"` + mediaBoundaryToken + `.png"}]}`, true},
		{"null", `{"inputs":null,"prompt":"` + mediaBoundaryToken + `"}`, true},
		{"wrong_type", `{"inputs":"` + mediaBoundaryToken + `"}`, true},
		{"escaped_key", `{"inpu\u0074s":{"capability":"` + mediaBoundaryToken + `"}}`, true},
		{"duplicate", `{"inputs":{"capability":"` + mediaBoundaryToken + `"},"inputs":null}`, true},
		{"oversized", `{"inputs":"` + strings.Repeat(mediaBoundaryToken, 3000) + `"}`, true},
		{"raw_nonobject", []byte(`[{"inputs":"` + mediaBoundaryToken + `"}]`), true},
		{"raw_malformed", []byte(`{"inputs":"` + mediaBoundaryToken), true},
		{"legacy_text", `{"prompt":"legacy-text"}`, false},
	}
}

// Capture actual go-redis commands before any connection/pool/DNS operation.
type mediaCommandHook struct{ commands [][]any }

func (h *mediaCommandHook) DialHook(goredis.DialHook) goredis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("offline retention fixture refuses all dialing")
	}
}

func (h *mediaCommandHook) ProcessHook(goredis.ProcessHook) goredis.ProcessHook {
	return func(_ context.Context, command goredis.Cmder) error {
		if command.Name() != "xadd" {
			return errors.New("offline retention fixture refuses unrelated commands")
		}
		h.commands = append(h.commands, append([]any(nil), command.Args()...))
		result, ok := command.(*goredis.StringCmd)
		if !ok {
			return errors.New("unexpected retained command result type")
		}
		result.SetVal("fixture-dlq-id")
		return nil
	}
}

func (h *mediaCommandHook) ProcessPipelineHook(goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(context.Context, []goredis.Cmder) error {
		return errors.New("offline retention fixture refuses pipelines")
	}
}

func mediaBoundaryJob(t *testing.T, client *Client, fixture mediaDLQFixture) *Job {
	t.Helper()
	job, err := client.parseMessage(goredis.XMessage{ID: "1-0", Values: map[string]any{
		"type": "MEDIA_GENERATE", "jobId": "fixture-job", "payload": fixture.payload,
		"enqueuedAt": "2026-10-22T00:00:00Z",
	}})
	if err != nil || job == nil {
		t.Fatalf("boundary fixture parse failed: %v", err)
	}
	return job
}

func checkMediaDLQFields(t *testing.T, fields map[string]any, private bool) {
	t.Helper()
	if fields["jobId"] != "fixture-job" || fields["original_message_id"] != "1-0" || fields["enqueuedAt"] != "2026-10-22T00:00:00Z" {
		t.Fatal("retained nonsecret source identity changed")
	}
	if private {
		if fields["payload"] != "null" {
			t.Fatal("retained DLQ contains a private payload")
		}
		for _, value := range fields {
			if text, ok := value.(string); ok && strings.Contains(text, mediaBoundaryToken) {
				t.Fatal("retained DLQ field contains a private input detail")
			}
		}
	} else {
		var retained map[string]any
		payload, _ := fields["payload"].(string)
		if json.Unmarshal([]byte(payload), &retained) != nil || retained["prompt"] != "legacy-text" || fields["reason"] != "legacy detail" {
			t.Fatal("ordinary legacy DLQ behavior changed")
		}
	}
}

func TestMediaDLQCommandRetentionBoundary(t *testing.T) {
	for _, fixture := range mediaDLQFixtures() {
		for _, multi := range []bool{false, true} {
			t.Run(fixture.name+map[bool]string{false: "/single", true: "/multi"}[multi], func(t *testing.T) {
				hook := &mediaCommandHook{}
				raw := goredis.NewClient(&goredis.Options{Addr: "fixture.invalid:6379"})
				t.Cleanup(func() { _ = raw.Close() })
				raw.AddHook(hook)
				client := &Client{client: raw, queueName: "jobs:v1:integration-test", workerID: "fixture-worker"}
				job := mediaBoundaryJob(t, client, fixture)
				var original []byte
				if fixture.private {
					original = job.PrivateMediaPayload().OriginalPayload()
				}
				reason := "legacy detail"
				if fixture.private {
					reason = "failed: " + mediaBoundaryToken
				}
				queue := "dlq:v1:integration-test"
				var err error
				if multi {
					queue = "dlq:v1:boundary:node:fixture"
					err = client.MoveToDLQFromQueue(context.Background(), "jobs:v1:boundary:node:fixture", job, reason)
				} else {
					err = client.MoveToDLQ(context.Background(), job, reason)
				}
				if err != nil || len(hook.commands) != 1 {
					t.Fatalf("offline command boundary failed: %v", err)
				}
				args := hook.commands[0]
				if len(args) < 3 || args[0] != "xadd" || args[1] != queue || args[2] != "*" {
					t.Fatal("DLQ destination or command changed")
				}
				fields := make(map[string]any)
				for i := 3; i+1 < len(args); i += 2 {
					key, ok := args[i].(string)
					if !ok {
						t.Fatal("unexpected retained field name")
					}
					fields[key] = args[i+1]
				}
				checkMediaDLQFields(t, fields, fixture.private)
				if fixture.private && !bytes.Equal(job.PrivateMediaPayload().OriginalPayload(), original) {
					t.Fatal("retention mutated original execution/digest bytes")
				}
			})
		}
	}
}

// Normal GitHub CI runs this existing in-process protocol fixture. The offline
// local test command deliberately selects CommandRetentionBoundary instead.
func TestMediaDLQStoredRetentionBoundary(t *testing.T) {
	for _, fixture := range mediaDLQFixtures() {
		for _, multi := range []bool{false, true} {
			t.Run(fixture.name+map[bool]string{false: "/single", true: "/multi"}[multi], func(t *testing.T) {
				_, client, raw := setupMiniredis(t)
				job := mediaBoundaryJob(t, client, fixture)
				reason := "legacy detail"
				if fixture.private {
					reason = "failed: " + mediaBoundaryToken
				}
				queue := "dlq:v1:integration-test"
				var err error
				if multi {
					queue = "dlq:v1:boundary:node:fixture"
					err = client.MoveToDLQFromQueue(context.Background(), "jobs:v1:boundary:node:fixture", job, reason)
				} else {
					err = client.MoveToDLQ(context.Background(), job, reason)
				}
				if err != nil {
					t.Fatal(err)
				}
				entries, err := raw.XRange(context.Background(), queue, "-", "+").Result()
				if err != nil || len(entries) != 1 {
					t.Fatal("missing stored DLQ entry")
				}
				checkMediaDLQFields(t, entries[0].Values, fixture.private)
			})
		}
	}
}
