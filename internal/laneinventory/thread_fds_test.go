package laneinventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThreadDescriptorPathProjection(t *testing.T) {
	home := filepath.Join(t.TempDir(), "owner")
	claude := filepath.Join(home, ".claude", "projects", "project-fixture", threadFixture+".jsonl")
	codex := filepath.Join(home, ".codex", "sessions", "2026", "01", "02", "rollout-2026-01-02T03-04-05-"+threadFixture+".jsonl")
	for _, tc := range []struct{ harness, path, want string }{
		{"claude", claude, threadFixture}, {"codex", codex, threadFixture},
		{"codex", claude, ""}, {"claude", codex, ""}, {"other", claude, ""},
		{"claude", strings.Replace(claude, "projects", "projects-extra", 1), ""},
		{"claude", strings.Replace(claude, "project-fixture", "project-fixture/subagents", 1), ""},
		{"claude", claude + " (deleted)", ""}, {"claude", claude + ".bak", ""},
		{"claude", strings.Replace(claude, threadFixture, "not-an-id", 1), ""},
		{"claude", filepath.Join(home, ".claude", "projects", threadFixture+".jsonl"), ""},
		{"claude", filepath.Join(home, ".claude", "projects", "project-fixture") + "/../project-fixture/" + threadFixture + ".jsonl", ""},
		{"codex", strings.Replace(codex, filepath.Join("2026", "01", "02"), "nested", 1), ""},
		{"codex", strings.Replace(codex, "rollout-2026-01-02T03-04-05-", "other-", 1), ""},
	} {
		if got := threadFromPath(home, tc.harness, tc.path); got != tc.want {
			t.Fatalf("%s projection: got %q, want %q", tc.harness, got, tc.want)
		}
	}
}

func fakeThreadFDs(t *testing.T, targets ...string) (string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home", "owner")
	dir := filepath.Join(root, "proc", "11", "fd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i, target := range targets {
		if err := os.Symlink(strings.ReplaceAll(target, "<HOME>", home), filepath.Join(dir, fmt.Sprint(i))); err != nil {
			t.Skip("symlinks unavailable")
		}
	}
	return filepath.Join(root, "proc"), home
}

func TestThreadDescriptorReadlinkOnly(t *testing.T) {
	// Targets do not exist. Success proves that no target content is opened.
	path := "<HOME>/.claude/projects/project-fixture/" + threadFixture + ".jsonl"
	proc, home := fakeThreadFDs(t, path, path, "pipe:[fixture]", "/unrelated/metadata.jsonl")
	id, err := readThreadFDs(context.Background(), proc, home, 11, "claude")
	if err != nil || id != threadFixture {
		t.Fatalf("got %q, %v", id, err)
	}
	proc, home = fakeThreadFDs(t, path, "<HOME>/.claude/projects/project-fixture/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
	if id, err = readThreadFDs(context.Background(), proc, home, 11, "claude"); id != "" || !errors.Is(err, ErrUnavailable) {
		t.Fatal("ambiguous descriptors were projected")
	}
	proc, home = fakeThreadFDs(t, "<HOME>/.codex/sessions/2026/01/02/rollout-2026-01-02T03-04-05-"+threadFixture+".jsonl")
	if id, err = readThreadFDs(context.Background(), proc, home, 11, "codex"); id != threadFixture || err != nil {
		t.Fatal("rollout descriptor not projected")
	}
}

func TestThreadDescriptorIdentityBinding(t *testing.T) {
	proc, home := fakeThreadFDs(t, "<HOME>/.claude/projects/project-fixture/"+threadFixture+".jsonl")
	for _, change := range []string{"none", "foreign-real", "foreign-effective", "reused-before", "reused-after", "unknown-birth", "identity-error"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			expected := Process{PID: 11, UID: 1000, StartedAt: 123}
			identity := func(context.Context) ([]int32, int64, error) {
				calls++
				uids := []int32{1000, 1000}
				started := int64(123)
				switch change {
				case "foreign-real":
					uids[0] = 2000
				case "foreign-effective":
					uids[1] = 2000
				case "reused-before":
					started++
				case "reused-after":
					if calls > 1 {
						started++
					}
				case "unknown-birth":
					started = 0
				case "identity-error":
					return nil, 0, errors.New("fixture private path")
				}
				return uids, started, nil
			}
			id, err := nativeThreadID(context.Background(), 1000, expected, home, proc, "claude", identity)
			if change == "none" {
				if err != nil || id != threadFixture || calls != 2 {
					t.Fatalf("valid identity: %q %v calls=%d", id, err, calls)
				}
			} else if id != "" || !errors.Is(err, ErrUnavailable) {
				t.Fatal("unverified identity projected")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := readThreadFDs(ctx, proc, home, 11, "claude"); id != "" || err == nil {
		t.Fatal("canceled scan returned metadata")
	}
}

func TestThreadDescriptorLimit(t *testing.T) {
	proc, home := fakeThreadFDs(t, "pipe:[fixture]")
	for i := 1; i < maxThreadFDs; i++ {
		if err := os.Symlink("pipe:[fixture]", filepath.Join(proc, "11", "fd", fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := readThreadFDs(context.Background(), proc, home, 11, "claude"); id != "" || err != nil {
		t.Fatal("exact descriptor cap refused")
	}
	if err := os.Symlink("pipe:[fixture]", filepath.Join(proc, "11", "fd", fmt.Sprint(maxThreadFDs))); err != nil {
		t.Fatal(err)
	}
	if id, err := readThreadFDs(context.Background(), proc, home, 11, "claude"); id != "" || err == nil {
		t.Fatal("descriptor cap was not enforced")
	}
}

func TestThreadDescriptorCollectorScopeAndBudget(t *testing.T) {
	c := fixture()
	c.Deps.Panes = func(context.Context) ([]RawPane, error) {
		return []RawPane{{PID: 10, ServerPID: 9, TTY: "/dev/pts/7", Terminal: true}}, nil
	}
	c.Deps.ThreadID = func(context.Context, Process, string) (string, error) {
		t.Fatal("managed terminal observed")
		return "", nil
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	c = fixture()
	c.Deps.Processes = func(context.Context) ([]Process, error) {
		out := []Process{{PID: 10, Parent: 9, UID: 1000, TTY: "/dev/pts/7", StartedAt: 123}}
		for i := 0; i < maxThreadProcesses; i++ {
			out = append(out, Process{PID: int32(100 + i), Parent: 10, UID: 1000, TTY: "/dev/pts/7", StartedAt: 123})
		}
		return out, nil
	}
	c.Deps.Argv = func(context.Context, Process) ([]string, error) { return []string{"claude"}, nil }
	calls := 0
	c.Deps.ThreadID = func(context.Context, Process, string) (string, error) { calls++; return threadFixture, nil }
	r, err := c.Collect(context.Background())
	if err != nil || calls != maxThreadProcesses || !r.Truncated || r.Panes[0].ThreadID != "" {
		t.Fatalf("budget not withheld: calls=%d report=%+v err=%v", calls, r, err)
	}
}

func TestThreadDescriptorCollector(t *testing.T) {
	c := fixture()
	c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
		if p.PID == 11 {
			return []string{"claude"}, nil
		}
		return []string{"helper"}, nil
	}
	calls := []int32{}
	c.Deps.ThreadID = func(_ context.Context, p Process, harness string) (string, error) {
		calls = append(calls, p.PID)
		if p.PID == 13 || harness != "claude" {
			t.Fatal("wrong process scope")
		}
		if p.PID == 12 {
			return threadFixture, nil
		}
		return "", nil
	}
	r, err := c.Collect(context.Background())
	if err != nil || len(r.Panes) != 1 || r.Panes[0].ThreadID != threadFixture || fmt.Sprint(calls) != "[11 12]" {
		t.Fatalf("fallback not wired: %+v %v calls=%v", r, err, calls)
	}
	raw, err := Encode(r)
	if err != nil || strings.Contains(string(raw), ".claude") || strings.Contains(string(raw), "/proc/") {
		t.Fatal("descriptor paths reached output")
	}
	c = fixture()
	c.Deps.ThreadID = func(context.Context, Process, string) (string, error) {
		t.Fatal("explicit argv id must win")
		return "", nil
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestThreadDescriptorCollectorUncertainty(t *testing.T) {
	for _, kind := range []string{"ambiguous", "error", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			c := fixture()
			c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
				if p.PID == 11 {
					return []string{"claude"}, nil
				}
				return []string{"helper"}, nil
			}
			c.Deps.ThreadID = func(_ context.Context, p Process, _ string) (string, error) {
				if p.PID == 11 {
					return threadFixture, nil
				}
				switch kind {
				case "ambiguous":
					return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil
				case "error":
					return "", errors.New("private metadata path")
				default:
					return "/private/path", nil
				}
			}
			r, err := c.Collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if r.Panes[0].ThreadID != "" {
				t.Fatal("uncertain id projected")
			}
			raw, err := Encode(r)
			if err != nil || !strings.Contains(string(raw), "thread_fds") || strings.Contains(string(raw), "private") {
				t.Fatal("unavailable observation not safely encoded")
			}
		})
	}
}
