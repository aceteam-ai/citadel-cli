package update_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

type channelSource struct {
	jobs    chan *worker.Job
	claimed chan string
}

func (s *channelSource) Name() string                  { return "memory" }
func (s *channelSource) Connect(context.Context) error { return nil }
func (s *channelSource) Next(ctx context.Context) (*worker.Job, error) {
	select {
	case job := <-s.jobs:
		s.claimed <- job.ID
		return job, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *channelSource) Ack(context.Context, *worker.Job) error                         { return nil }
func (s *channelSource) Nack(context.Context, *worker.Job, error) error                 { return nil }
func (s *channelSource) Fail(context.Context, *worker.Job, error, map[string]any) error { return nil }
func (s *channelSource) IsJobCancelled(context.Context, string) bool                    { return false }
func (s *channelSource) Close() error                                                   { return nil }

type channelHandler struct {
	started      chan string
	firstRelease chan struct{}
}

func (h *channelHandler) CanHandle(jobType string) bool { return jobType == "TEST" }
func (h *channelHandler) Execute(ctx context.Context, job *worker.Job, _ worker.StreamWriter) (*worker.JobResult, error) {
	h.started <- job.ID
	if job.ID == "first" && h.firstRelease != nil {
		select {
		case <-h.firstRelease:
		case <-ctx.Done():
		}
	}
	return &worker.JobResult{Status: worker.JobStatusSuccess}, nil
}

type runnerChecker struct{ err error }

func (c runnerChecker) CheckForUpdate() (*update.Release, error) {
	return &update.Release{TagName: "v9.9.9"}, nil
}
func (c runnerChecker) DownloadAndVerify(*update.Release, string) error { return c.err }

func receive(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("started job %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("job %q did not start", want)
	}
}

func TestAutoUpdaterAbortsResumeRealRunner(t *testing.T) {
	for _, scenario := range []string{"idle timeout", "apply failure", "restart failure", "permanent drain", "cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			source := &channelSource{jobs: make(chan *worker.Job, 2), claimed: make(chan string, 2)}
			handler := &channelHandler{started: make(chan string, 2)}
			if scenario == "idle timeout" || scenario == "cancellation" {
				handler.firstRelease = make(chan struct{})
			}
			runner := worker.NewRunner(source, []worker.JobHandler{handler}, worker.RunnerConfig{
				WorkerID: "test", State: worker.NewWorkerState(), ActivityFn: func(string, string) {},
			})
			ctx, cancel := context.WithCancel(context.Background())
			runnerDone := make(chan struct{})
			go func() { _ = runner.Run(ctx); close(runnerDone) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-runnerDone:
				case <-time.After(3 * time.Second):
					t.Error("runner did not stop")
				}
			})
			source.jobs <- &worker.Job{ID: "first", Type: "TEST"}
			receive(t, handler.started, "first")
			receive(t, source.claimed, "first")
			if scenario != "idle timeout" && scenario != "cancellation" {
				// Wait until the first handler has finished before triggering update.
				deadline := time.After(3 * time.Second)
				for runner.ActiveJobs() != 0 {
					select {
					case <-deadline:
						t.Fatal("first job did not finish")
					default:
					}
					select {
					case <-time.After(time.Millisecond):
					case <-deadline:
						t.Fatal("first job did not finish")
					}
				}
			}
			var now atomic.Int64
			now.Store(1)
			ticks := make(chan time.Time)
			idleTicks := make(chan time.Time)
			paused := make(chan struct{})
			released := make(chan struct{})
			entered := make(chan struct{})
			continueAttempt := make(chan struct{})
			u := update.NewAutoUpdater(update.AutoUpdaterConfig{
				Checker: runnerChecker{}, Ticks: ticks, IdleTicks: idleTicks,
				Now:         func() time.Time { return time.Unix(0, now.Load()) },
				IdleTimeout: time.Second, ActiveJobs: runner.ActiveJobs,
				BeginDrain: func() func() {
					release := runner.BeginDrain()
					close(paused)
					return func() { release(); close(released) }
				},
				Apply: func(string) error {
					close(entered)
					<-continueAttempt
					if scenario == "apply failure" || scenario == "permanent drain" {
						return errors.New("apply failed")
					}
					return nil
				},
				Restart: func() error {
					if scenario == "restart failure" {
						return errors.New("restart failed")
					}
					return nil
				},
			})
			updaterDone := make(chan struct{})
			go func() { u.Run(ctx); close(updaterDone) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-updaterDone:
				case <-time.After(3 * time.Second):
					t.Error("updater did not stop")
				}
			})
			ticks <- time.Time{}
			select {
			case <-paused:
			case <-time.After(3 * time.Second):
				t.Fatal("updater did not drain")
			}
			if !runner.IsDraining() {
				t.Fatal("runner did not pause")
			}
			source.jobs <- &worker.Job{ID: "second", Type: "TEST"}
			select {
			case got := <-source.claimed:
				t.Fatalf("job %q was claimed during updater pause", got)
			case <-time.After(30 * time.Millisecond):
			}
			select {
			case got := <-handler.started:
				t.Fatalf("job %q started during updater pause", got)
			default:
			}
			if scenario == "permanent drain" {
				runner.Drain()
			}
			if scenario == "cancellation" {
				cancel()
				select {
				case <-released:
				case <-time.After(3 * time.Second):
					t.Fatal("cancelled attempt did not release its scope")
				}
				select {
				case <-runnerDone:
				case <-time.After(3 * time.Second):
					t.Fatal("cancelled runner did not stop")
				}
				if !runner.IsDraining() {
					t.Fatal("shutdown did not establish permanent drain")
				}
				select {
				case got := <-handler.started:
					t.Fatalf("job %q started after cancellation", got)
				default:
				}
				return
			}
			if scenario == "idle timeout" {
				now.Store(int64(2 * time.Second))
				idleTicks <- time.Time{}
				select {
				case <-released:
				case <-time.After(3 * time.Second):
					t.Fatal("timed-out attempt did not release its drain")
				}
				close(handler.firstRelease)
			} else {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("apply did not begin")
				}
				close(continueAttempt)
			}
			if scenario == "permanent drain" {
				select {
				case <-released:
				case <-time.After(3 * time.Second):
					t.Fatal("aborted updater did not release its scope")
				}
				if !runner.IsDraining() {
					t.Fatal("updater release cleared permanent drain")
				}
				cancel()
				select {
				case <-runnerDone:
				case <-time.After(3 * time.Second):
					t.Fatal("runner did not stop")
				}
				select {
				case got := <-handler.started:
					t.Fatalf("job %q started despite permanent drain", got)
				default:
				}
				return
			}
			// The next job must run on the same old process after an abort.
			receive(t, handler.started, "second")
			receive(t, source.claimed, "second")
			if scenario == "restart failure" {
				state, err := update.LoadState()
				if err != nil {
					t.Fatal(err)
				}
				if state.CurrentVersion != "v9.9.9" {
					t.Fatalf("staged version lost: %+v", state)
				}
			}
		})
	}
}
