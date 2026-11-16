package laneinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

const threadFixture = "019a8f2c-1234-4567-8abc-0123456789ab"

func fixture() Collector {
	return Collector{Deps: Deps{
		UID: 1000,
		Panes: func(context.Context) ([]RawPane, error) {
			return []RawPane{{Session: "lane", Window: "@1", Target: "%2", Command: "bash", Cwd: "/fixture/repo", PID: 10}}, nil
		},
		Processes: func(context.Context) ([]Process, error) {
			return []Process{{PID: 10, UID: 1000, StartedAt: 123}, {PID: 11, Parent: 10, UID: 1000}, {PID: 12, Parent: 11, UID: 1000}, {PID: 13, Parent: 10, UID: 2000}}, nil
		},
		Argv: func(_ context.Context, pid int32) ([]string, error) {
			if pid == 12 {
				return []string{"/usr/bin/codex", "resume", threadFixture, "--api-key", "planted-opaque-secret"}, nil
			}
			return []string{"bash"}, nil
		},
		Declarations: func(context.Context) (map[int32]string, error) { return map[int32]string{11: "codex-fixture"}, nil },
		Git: func(context.Context, string) (*Worktree, error) {
			n := uint64(3)
			return &Worktree{Path: "/fixture/repo", Remotes: []string{"origin"}, Branch: "topic", Head: strings.Repeat("a", 40), Ahead: &n, DirtyFiles: 2}, nil
		},
		Capacity: func(context.Context) (Capacity, error) {
			n := uint64(512)
			return Capacity{NCPU: 4, MemoryAvailable: &n, DiskAvailable: &n}, nil
		},
	}}
}

func TestInventoryProjection(t *testing.T) {
	c := fixture()
	called := []int32{}
	argv := c.Deps.Argv
	c.Deps.Argv = func(ctx context.Context, pid int32) ([]string, error) {
		called = append(called, pid)
		if pid == 13 {
			t.Fatal("foreign user argv read")
		}
		return argv(ctx, pid)
	}
	r, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Panes) != 1 || len(called) != 3 {
		t.Fatalf("panes=%d argv calls=%v", len(r.Panes), called)
	}
	p := r.Panes[0]
	if p.Harness != "codex" || p.ThreadID != threadFixture || p.DeclaredSession != "codex-fixture" || p.StartedAt == nil || *p.StartedAt != 123 {
		t.Fatalf("projection %+v", p)
	}
	if p.Worktree == nil || p.Worktree.DirtyFiles != 2 || p.Worktree.Ahead == nil || *p.Worktree.Ahead != 3 {
		t.Fatalf("worktree %+v", p.Worktree)
	}
	raw, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"planted-opaque-secret", "api-key", "argv", "file_contents"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("private input %q reached output", secret)
		}
	}
	if len(raw)+1 > MaxBytes || !json.Valid(raw) {
		t.Fatal("invalid or unbounded output")
	}
}

func TestInventoryRedactsAllPanesAfterObservation(t *testing.T) {
	c := fixture()
	c.Deps.Panes = func(context.Context) ([]RawPane, error) {
		return []RawPane{{Session: "planted-opaque-secret", Cwd: "/fixture/planted-opaque-secret", PID: 10}, {Session: "second", Cwd: "/fixture/repo", PID: 11}}, nil
	}
	c.Deps.Git = func(context.Context, string) (*Worktree, error) {
		return &Worktree{Path: "/fixture", Remotes: []string{"act_abcdefgh1234567890"}, Branch: "planted-opaque-secret"}, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Encode(r)
	if strings.Contains(string(raw), "planted-opaque-secret") || strings.Contains(string(raw), "act_abcdefgh1234567890") {
		t.Fatalf("secret in report %s", raw)
	}
	if !strings.Contains(string(raw), "REDACTED") {
		t.Fatal("redaction not exercised")
	}
}

func TestInventoryHarness(t *testing.T) {
	for _, tc := range []struct {
		args            []string
		harness, thread string
	}{
		{[]string{"bash", "-c", "codex resume " + threadFixture}, "other", ""},
		{[]string{"codex", "resume", threadFixture}, "codex", threadFixture},
		{[]string{"node", "/package/codex.js", "exec", "resume", threadFixture}, "codex", threadFixture},
		{[]string{"claude", "--session-id=" + threadFixture}, "claude", threadFixture},
		{[]string{"node", "/pkg/@anthropic-ai/claude-code/cli.js", "--resume", threadFixture}, "claude", threadFixture},
		{[]string{"claude", "--resume", "planted-opaque-secret"}, "claude", ""},
		{[]string{"qwen"}, "qwen", ""}, {[]string{"ace"}, "ace", ""},
		{nil, "other", ""},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			h, id := Detect(tc.args)
			if h != tc.harness || id != tc.thread {
				t.Fatalf("got %q/%q", h, id)
			}
		})
	}
}

func TestInventoryEncodeRedactsAndDoesNotMutate(t *testing.T) {
	secret := "act_abcdefgh1234567890"
	r := Report{Panes: []Pane{{Session: secret, ThreadID: secret, Harness: secret, Worktree: &Worktree{Path: secret, Remotes: []string{secret}}}}, Unavailable: []string{secret}}
	raw, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("publication leaked a shaped credential: %s", raw)
	}
	if r.Panes[0].Session != secret || r.Panes[0].Worktree.Remotes[0] != secret {
		t.Fatal("encoding mutated cached report")
	}
}

func TestInventoryOptionalCapacityUnknown(t *testing.T) {
	nan, infinite := math.NaN(), math.Inf(1)
	raw, err := Encode(Report{Capacity: Capacity{NCPU: 2, Load1: &nan, Load5: &infinite}})
	if err != nil {
		t.Fatal("optional capacity rejected inventory")
	}
	var result Report
	if json.Unmarshal(raw, &result) != nil || result.Capacity.Load1 != nil || result.Capacity.Load5 != nil || strings.Join(result.Unavailable, ",") != "capacity" {
		t.Fatalf("capacity not unknown: %s", raw)
	}
}

func TestInventoryCredentialShapeAndUUIDSecret(t *testing.T) {
	if got := clean("act_abc\ndefgh1234567890", nil); strings.Contains(got, "abcdefgh") {
		t.Fatal("normalization assembled an unredacted bearer")
	}
	if got := clean("https://user:password@host.invalid/private", nil); strings.Contains(got, "password") {
		t.Fatal("URL credential leaked")
	}
	c := fixture()
	c.Deps.Argv = func(context.Context, int32) ([]string, error) {
		return []string{"codex", "resume", threadFixture, "--api-key", threadFixture}, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Encode(r)
	if strings.Contains(string(raw), threadFixture) {
		t.Fatal("UUID-shaped explicit credential leaked as thread identity")
	}
}

func TestInventoryDirtyStatusBounds(t *testing.T) {
	for _, value := range [][]byte{[]byte(" M filename"), []byte("R  target\x00"), []byte("x\x00"), []byte(" M " + strings.Repeat("x", maxProbeBytes) + "\x00")} {
		if _, err := dirtyCount(value); err == nil {
			t.Fatal("malformed or oversized dirty status accepted")
		}
	}
}

func TestInventoryPartialAndCancellation(t *testing.T) {
	c := fixture()
	c.Deps.Git = func(context.Context, string) (*Worktree, error) { return nil, errors.New("credential in probe error") }
	c.Deps.Declarations = nil
	c.Deps.Capacity = nil
	r, err := c.Collect(context.Background())
	if err != nil || strings.Join(r.Unavailable, ",") != "capacity,declarations,git" {
		t.Fatalf("partial %+v %v", r, err)
	}
	raw, _ := Encode(r)
	if strings.Contains(string(raw), "credential") {
		t.Fatal("error text escaped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Collect(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cancelled collection succeeded")
	}
}

func TestInventoryBounds(t *testing.T) {
	if MaxBytes != 65536 || MaxPanes != 128 || MaxProcesses != 4096 || MaxArgBytes != 32768 {
		t.Fatal("unreviewed bound change")
	}
	c := fixture()
	c.Deps.Panes = func(context.Context) ([]RawPane, error) {
		out := []RawPane{}
		for i := 0; i < MaxPanes+1; i++ {
			out = append(out, RawPane{Session: strings.Repeat("a", 4096), PID: 10})
		}
		return out, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Panes) != MaxPanes || !r.Truncated {
		t.Fatal("pane cap lost")
	}
	raw, err := Encode(r)
	if err != nil || len(raw)+1 > MaxBytes {
		t.Fatalf("encoded %d %v", len(raw), err)
	}
	var clipped Report
	if json.Unmarshal(raw, &clipped) != nil || !clipped.Truncated || len(clipped.Panes) >= MaxPanes {
		t.Fatal("output not structurally truncated")
	}
	c = fixture()
	c.Deps.Argv = func(context.Context, int32) ([]string, error) {
		return []string{"codex", "resume", threadFixture, strings.Repeat("x", MaxArgBytes)}, nil
	}
	r, err = c.Collect(context.Background())
	if err != nil || r.Panes[0].Harness != "other" || !strings.Contains(strings.Join(r.Unavailable, ","), "argv") {
		t.Fatal("oversize argv accepted")
	}
}

func TestInventoryParsePanes(t *testing.T) {
	line := "lane\x1f@1\x1f%2\x1fbash\x1f/fixture/repo\x1f42\n"
	panes, err := parsePanes([]byte(line))
	if err != nil || len(panes) != 1 || panes[0].PID != 42 || panes[0].Target != "%2" {
		t.Fatalf("parse %+v %v", panes, err)
	}
	for _, raw := range []string{"bad\n", strings.ReplaceAll(line, "42", "bad"), strings.Replace(line, "/fixture/repo", "/"+strings.Repeat("x", maxProbeBytes), 1)} {
		if _, err := parsePanes([]byte(raw)); err == nil {
			t.Fatal("malformed pane accepted")
		}
	}
}

func TestInventoryGitMetadataOnly(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(fmt.Sprint(detached), func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls++
				if name != "git" || strings.Join(args[:8], " ") != "--no-optional-locks -c core.fsmonitor=false -c core.untrackedCache=false -C /fixture/repo rev-parse" && calls < 3 {
					t.Fatalf("unexpected reader %s %v", name, args)
				}
				command := strings.Join(args[7:], " ")
				switch command {
				case "rev-parse --show-toplevel":
					return []byte("/fixture/repo\n"), nil
				case "rev-parse --verify HEAD":
					return []byte(strings.Repeat("b", 40) + "\n"), nil
				case "rev-parse --abbrev-ref HEAD":
					if detached {
						return []byte("HEAD\n"), nil
					}
					return []byte("topic\n"), nil
				case "remote":
					return []byte("origin\nbackup\n"), nil
				case "status --porcelain=v1 -z --untracked-files=normal":
					return []byte(" M SECRET-FILENAME\x00R  renamed\x00old-secret\x00?? another-secret\x00"), nil
				case "rev-list --count @{upstream}..HEAD":
					if detached {
						return nil, ErrUnavailable
					}
					return []byte("2\n"), nil
				}
				t.Fatalf("forbidden git operation %v", args)
				return nil, ErrUnavailable
			}
			w, err := readGit(context.Background(), run, "/fixture/repo")
			if err != nil {
				t.Fatal(err)
			}
			if w.DirtyFiles != 3 || w.Detached != detached || len(w.Remotes) != 2 || (!detached && (w.Ahead == nil || *w.Ahead != 2)) || (detached && w.Ahead != nil) {
				t.Fatalf("summary %+v", w)
			}
			raw, _ := json.Marshal(w)
			for _, secret := range []string{"SECRET-FILENAME", "old-secret", "another-secret"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("filename leaked")
				}
			}
		})
	}
}
