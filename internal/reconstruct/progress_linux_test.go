//go:build linux

package reconstruct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProgressIsBoundedStaticAndOrdered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		video bool
		mode  string
		want  []ProgressEvent
	}{
		{"video-success", true, "success", []ProgressEvent{
			{"extract_frames", "starting"}, {"extract_frames", "complete"},
			{"sfm", "starting"}, {"sfm", "complete"},
			{"training", "starting"}, {"training", "complete"},
		}},
		{"frames-success", false, "success", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "complete"},
			{"training", "starting"}, {"training", "complete"},
		}},
		{"extraction-error", true, "extract-error", []ProgressEvent{
			{"extract_frames", "starting"}, {"extract_frames", "failed"},
		}},
		{"sfm-error", false, "sfm-error", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "failed"},
		}},
		{"sfm-invalid-signal", false, "invalid-signal", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "failed"},
		}},
		{"sfm-quality-rejected", false, "quality-rejected", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "rejected"},
		}},
		{"training-error", false, "training-error", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "complete"},
			{"training", "starting"}, {"training", "failed"},
		}},
		{"training-cancelled", false, "cancelled", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "complete"},
			{"training", "starting"}, {"training", "failed"},
		}},
		{"training-done-is-not-job-success", false, "missing-artifact", []ProgressEvent{
			{"sfm", "starting"}, {"sfm", "complete"},
			{"training", "starting"}, {"training", "complete"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const marker = "private-input-or-tool-output"
			root := t.TempDir()
			input, output := filepath.Join(root, marker), filepath.Join(root, "result")
			if tc.video {
				if err := os.WriteFile(input, []byte("synthetic-video"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(input, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []ProgressEvent
			fake := successfulFake(t, []byte(marker))
			original := fake.run
			fake.run = func(ctx context.Context, invocation Invocation, index int) error {
				// Large raw/private stdout and stderr do not become progress.
				_, _ = io.WriteString(invocation.Stdout, strings.Repeat(marker+"\n", 20_000))
				_, _ = io.WriteString(invocation.Stderr, marker+"\n")
				if invocation.Name == "ffmpeg" {
					if tc.mode == "extract-error" {
						return fmt.Errorf("failed at %s", marker)
					}
					return nil
				}
				if invocation.Args[0] == "sfm" {
					switch tc.mode {
					case "sfm-error":
						return fmt.Errorf("failed at %s", marker)
					case "invalid-signal":
						return nil
					case "quality-rejected":
						_, _ = io.WriteString(invocation.Stdout,
							"Registered: 1/10 images\nReprojection error: mean 0.5 px\n")
						return nil
					}
				}
				if invocation.Args[0] == "train" {
					switch tc.mode {
					case "training-error":
						return fmt.Errorf("failed at %s", marker)
					case "cancelled":
						cancel()
						return ctx.Err()
					case "missing-artifact":
						return nil
					}
				}
				return original(ctx, invocation, index)
			}
			result, err := (Runner{
				Executor: fake,
				Progress: func(event ProgressEvent) { events = append(events, event) },
			}).Run(ctx, Request{InputPath: input, OutputDir: output, Iterations: 3})
			if !reflect.DeepEqual(events, tc.want) || len(events) > 6 {
				t.Fatalf("events=%+v want=%+v", events, tc.want)
			}
			raw, jsonErr := json.Marshal(events)
			if jsonErr != nil || strings.Contains(string(raw), marker) || strings.Contains(string(raw), root) {
				t.Fatal("progress exposed tool output or input path")
			}
			if tc.mode == "success" {
				if err != nil || result.Artifact == nil {
					t.Fatalf("synthetic success failed: %v", err)
				}
			} else {
				if err == nil || result.Artifact != nil {
					t.Fatal("failed run reported a published artifact")
				}
				if tc.mode == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation identity lost: %v", err)
				}
				assertNoPublishedOrPartial(t, output)
			}
		})
	}
}

func TestInvalidRequestEmitsNoProgress(t *testing.T) {
	var events []ProgressEvent
	fake := &fakeExecutor{}
	_, err := (Runner{
		Executor: fake,
		Progress: func(event ProgressEvent) { events = append(events, event) },
	}).Run(context.Background(), Request{})
	if err == nil || len(events) != 0 || len(fake.Calls()) != 0 {
		t.Fatal("invalid request emitted progress or executed a child")
	}
}
