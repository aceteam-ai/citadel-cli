package laneinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

const threadFixture = "019a8f2c-1234-4567-8abc-0123456789ab"

func fixture() Collector {
	return Collector{Deps: Deps{
		UID: 1000,
		Panes: func(context.Context) ([]RawPane, error) {
			return []RawPane{{Session: "lane", Window: "@1", Target: "%2", Command: "bash", Cwd: "/fixture/repo", PID: 10, ServerPID: 9, TTY: "/dev/pts/7"}}, nil
		},
		Processes: func(context.Context) ([]Process, error) {
			return []Process{{PID: 10, Parent: 9, UID: 1000, StartedAt: 123, TTY: "/dev/pts/7"}, {PID: 11, Parent: 10, UID: 1000, TTY: "/dev/pts/7"}, {PID: 12, Parent: 11, UID: 1000, TTY: "/dev/pts/7"}, {PID: 13, Parent: 10, UID: 2000, TTY: "/dev/pts/7"}}, nil
		},
		Argv: func(_ context.Context, p Process) ([]string, error) {
			if p.PID == 12 {
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
	c.Deps.Argv = func(ctx context.Context, p Process) ([]string, error) {
		called = append(called, p.PID)
		if p.PID == 13 {
			t.Fatal("foreign user argv read")
		}
		return argv(ctx, p)
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
		return []RawPane{{Session: "planted-opaque-secret", Cwd: "/fixture/planted-opaque-secret", PID: 10, ServerPID: 9, TTY: "/dev/pts/7"}, {Session: "second", Cwd: "/fixture/repo", PID: 20, ServerPID: 9, TTY: "/dev/pts/7"}}, nil
	}
	c.Deps.Processes = func(context.Context) ([]Process, error) {
		return []Process{{PID: 10, Parent: 9, UID: 1000, TTY: "/dev/pts/7"}, {PID: 20, Parent: 9, UID: 1000, TTY: "/dev/pts/7"}}, nil
	}
	c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
		if p.PID == 20 {
			return []string{"codex", "--api-key", "planted-opaque-secret"}, nil
		}
		return []string{"bash"}, nil
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

func TestInventoryEncodeRemoteAndHeadBounds(t *testing.T) {
	if maxRemotes != 32 || maxRemoteBytes != 128 {
		t.Fatal("unreviewed remote limits")
	}
	remotes := make([]string, maxRemotes+1)
	for i := range remotes {
		remotes[i] = fmt.Sprintf("remote-%d", i)
	}
	for _, tc := range []struct {
		name        string
		remotes     []string
		head        string
		wantRemotes int
		clipped     bool
		validHead   bool
	}{
		{"count", remotes, strings.Repeat("a", 40), 32, true, true},
		{"at-name-bound", []string{strings.Repeat("&", 128)}, strings.Repeat("B", 64), 1, false, true},
		{"over-name-bound", []string{strings.Repeat("&", 129)}, strings.Repeat("c", 40), 0, true, true},
		{"non-hex-head", []string{"origin"}, strings.Repeat("z", 40), 1, false, false},
		{"short-head", []string{"origin"}, "abc123", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := Report{Panes: []Pane{{Worktree: &Worktree{Remotes: tc.remotes, Head: tc.head}}}}
			raw, err := Encode(input)
			var out Report
			if err != nil || json.Unmarshal(raw, &out) != nil || len(out.Panes) != 1 {
				t.Fatalf("encoding: %v", err)
			}
			w := out.Panes[0].Worktree
			if len(w.Remotes) != tc.wantRemotes || out.Truncated != tc.clipped || (w.Head == tc.head) != tc.validHead {
				t.Fatalf("remotes=%d head=%q truncated=%v", len(w.Remotes), w.Head, out.Truncated)
			}
			if !tc.validHead && (w.Head != "" || strings.Join(out.Unavailable, ",") != "git") {
				t.Fatal("invalid head not unknown")
			}
			if input.Panes[0].Worktree.Head != tc.head || len(input.Panes[0].Worktree.Remotes) != len(tc.remotes) {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestInventoryEncodeShedsOffendingRow(t *testing.T) {
	escaped := strings.Repeat("&", 4096)
	bad := Pane{Session: escaped, Window: escaped, Target: escaped, Command: escaped, Cwd: escaped, DeclaredSession: escaped}
	for _, panes := range [][]Pane{{bad, {Session: "healthy"}}, {{Session: "healthy"}, bad}} {
		raw, err := Encode(Report{Panes: panes})
		var out Report
		if err != nil || json.Unmarshal(raw, &out) != nil || len(raw)+1 > MaxBytes {
			t.Fatalf("encoding: %v", err)
		}
		if len(out.Panes) != 1 || out.Panes[0].Session != "healthy" || !out.Truncated {
			t.Fatal("oversized row erased healthy pane")
		}
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
	c.Deps.Argv = func(context.Context, Process) ([]string, error) {
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
			out = append(out, RawPane{Session: strings.Repeat("a", 4096), PID: 10, ServerPID: 9, TTY: "/dev/pts/7"})
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
	c.Deps.Argv = func(context.Context, Process) ([]string, error) {
		return []string{"codex", "resume", threadFixture, strings.Repeat("x", MaxArgBytes)}, nil
	}
	r, err = c.Collect(context.Background())
	if err != nil || r.Panes[0].Harness != "other" || !strings.Contains(strings.Join(r.Unavailable, ","), "argv") {
		t.Fatal("oversize argv accepted")
	}
}

func TestInventoryParsePanes(t *testing.T) {
	line := "9|42|@1|%2|/dev/pts/7|0\n"
	panes, err := parsePanes([]byte(line))
	if err != nil || len(panes) != 1 || panes[0].PID != 42 || panes[0].Target != "%2" {
		t.Fatalf("parse %+v %v", panes, err)
	}
	for _, raw := range []string{"bad\n", strings.ReplaceAll(line, "42", "bad"), strings.Replace(line, "/dev/pts/7", "/dev/pts/"+strings.Repeat("1", maxProbeBytes), 1), line + line} {
		if _, err := parsePanes([]byte(raw)); err == nil {
			t.Fatal("malformed pane accepted")
		}
	}
}

func TestInventoryTerminalScope(t *testing.T) {
	c := fixture()
	c.Deps.Panes = func(context.Context) ([]RawPane, error) {
		return []RawPane{
			{Session: "citadel-alice", PID: 10, Cwd: "/private/alice", Terminal: true},
			{Session: "custom-bob", PID: 20, Cwd: "/private/bob", Terminal: true},
			{Session: "node-owner-lane", PID: 30, Cwd: "/fixture/repo", ServerPID: 9, TTY: "/dev/pts/7"},
		}, nil
	}
	c.Deps.Processes = func(context.Context) ([]Process, error) {
		return []Process{{PID: 10, UID: 1000}, {PID: 11, Parent: 10, UID: 1000, TTY: "/dev/pts/7"}, {PID: 20, UID: 1000}, {PID: 21, Parent: 20, UID: 1000}, {PID: 30, Parent: 9, UID: 1000, TTY: "/dev/pts/7"}}, nil
	}
	c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
		if p.PID != 30 {
			t.Fatal("terminal argv observed without verified caller")
		}
		return []string{"codex"}, nil
	}
	c.Deps.Git = func(_ context.Context, cwd string) (*Worktree, error) {
		if cwd != "/fixture/repo" {
			t.Fatal("terminal worktree observed without verified caller")
		}
		return nil, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil || len(r.Panes) != 1 || r.Panes[0].Session != "node-owner-lane" {
		t.Fatal("terminal scope not filtered")
	}
	raw, err := Encode(r)
	if err != nil || strings.Contains(string(raw), "alice") || strings.Contains(string(raw), "bob") || !strings.Contains(string(raw), "terminal_scope") {
		t.Fatal("terminal metadata published")
	}
	// A blocked terminal subtree must stay blocked even if a fixture lists one
	// of its descendants separately as an otherwise ordinary pane.
	panes := c.Deps.Panes
	c.Deps.Panes = func(ctx context.Context) ([]RawPane, error) {
		raw, err := panes(ctx)
		// A nested tmux root otherwise has valid server/TTY metadata. It must
		// still be excluded through the blocked terminal ancestor.
		return append(raw, RawPane{Session: "descendant", PID: 11, ServerPID: 10, TTY: "/dev/pts/7", Cwd: "/private/alice"}), err
	}
	r, err = c.Collect(context.Background())
	if err != nil || len(r.Panes) != 1 {
		t.Fatal("terminal descendant not filtered")
	}
}

func TestInventoryServingProcessAndTTYBoundaries(t *testing.T) {
	for _, boundary := range []string{"serving-process", "different-tty", "unknown-tty"} {
		t.Run(boundary, func(t *testing.T) {
			self := int32(os.Getpid())
			root, foreign, child := self+100, self+200, self+300
			middle, tty := self, "/dev/pts/7"
			if boundary != "serving-process" {
				middle = root
				tty = "/dev/pts/8"
			}
			if boundary == "unknown-tty" {
				tty = ""
			}
			c := fixture()
			c.Deps.Panes = func(context.Context) ([]RawPane, error) {
				return []RawPane{{Session: "ops", PID: root, ServerPID: 9, TTY: "/dev/pts/7"}}, nil
			}
			c.Deps.Processes = func(context.Context) ([]Process, error) {
				out := []Process{{PID: root, Parent: 9, UID: 1000, TTY: "/dev/pts/7"}, {PID: foreign, Parent: middle, UID: 1000, TTY: tty}, {PID: child, Parent: foreign, UID: 1000, TTY: tty}}
				if boundary == "serving-process" {
					out = append(out, Process{PID: self, Parent: root, UID: 1000, TTY: "/dev/pts/7"})
				}
				return out, nil
			}
			c.Deps.Declarations = func(context.Context) (map[int32]string, error) {
				return map[int32]string{foreign: "private-declaration"}, nil
			}
			c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
				if p.PID != root {
					t.Fatal("walk crossed the serving-process or pane TTY boundary")
				}
				return []string{"bash"}, nil
			}
			r, err := c.Collect(context.Background())
			if err != nil || len(r.Panes) != 1 || r.Panes[0].Harness != "other" || r.Panes[0].ThreadID != "" || r.Panes[0].DeclaredSession != "" {
				t.Fatal("foreign terminal projected")
			}
		})
	}
}

func TestInventoryTerminalAfterPaneCap(t *testing.T) {
	c := fixture()
	c.Deps.Panes = func(context.Context) ([]RawPane, error) {
		out := []RawPane{}
		for i := 0; i < MaxPanes; i++ {
			out = append(out, RawPane{PID: 10, ServerPID: 9, TTY: "/dev/pts/7"})
		}
		return append(out, RawPane{PID: 20, Terminal: true}), nil
	}
	c.Deps.Processes = func(context.Context) ([]Process, error) {
		return []Process{{PID: 10, Parent: 9, UID: 1000, TTY: "/dev/pts/7"}, {PID: 20, Parent: 10, UID: 1000, TTY: "/dev/pts/7"}, {PID: 21, Parent: 20, UID: 1000, TTY: "/dev/pts/7"}}, nil
	}
	c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
		if p.PID != 10 {
			t.Fatal("terminal beyond display cap was walked")
		}
		return []string{"bash"}, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil || !r.Truncated || len(r.Panes) != MaxPanes || !strings.Contains(strings.Join(r.Unavailable, ","), "terminal_scope") {
		t.Fatal("full terminal coverage lost before truncation")
	}
}

func TestInventoryPaneParentAndTTY(t *testing.T) {
	for _, tc := range []struct {
		server, parent      int32
		paneTTY, processTTY string
	}{{0, 9, "/dev/pts/7", "/dev/pts/7"}, {9, 8, "/dev/pts/7", "/dev/pts/7"}, {9, 9, "", "/dev/pts/7"}, {9, 9, "/dev/pts/7", "/dev/pts/8"}} {
		c := fixture()
		c.Deps.Panes = func(context.Context) ([]RawPane, error) {
			return []RawPane{{PID: 10, ServerPID: tc.server, TTY: tc.paneTTY}}, nil
		}
		c.Deps.Processes = func(context.Context) ([]Process, error) {
			return []Process{{PID: 10, Parent: tc.parent, UID: 1000, TTY: tc.processTTY}}, nil
		}
		c.Deps.Argv = func(context.Context, Process) ([]string, error) {
			t.Fatal("unverified pane argv read")
			return nil, nil
		}
		r, err := c.Collect(context.Background())
		if err != nil || len(r.Panes) != 0 || !strings.Contains(strings.Join(r.Unavailable, ","), "pane_process") {
			t.Fatal("pane parent or TTY check missing")
		}
	}
}

func TestInventoryTerminalMarkerParsing(t *testing.T) {
	for _, tc := range []struct {
		name, marker, base string
		blocked            bool
	}{
		{"lane", "", "", false},
		{"citadel-alice", "", "", true},
		{"citadel", "", "", true},
		{"custom-bob", "citadel-v1", "", true},
		{"custom-bob", "unknown-marker", "", true},
		{"custom-bob", "", "custom", true},
		{"node-citadel-lane", "", "", false},
	} {
		t.Run(tc.name+tc.marker+tc.base, func(t *testing.T) {
			panes, err := readPanes(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "-N" {
					t.Fatal("probe may start a server")
				}
				if args[0] == "list-panes" {
					flag := "0"
					if tc.marker != "" {
						flag = "1"
					}
					return []byte("9|10|@1|%2|/dev/pts/7|" + flag + "\n"), nil
				}
				if args[len(args)-1] == "#{session_name}" {
					return []byte(tc.name + "\n"), nil
				}
				return []byte("fixture\n"), nil
			}, tc.base)
			if err != nil || len(panes) != 1 || panes[0].Terminal != tc.blocked {
				t.Fatal("terminal classification mismatch")
			}
		})
	}
	if !strings.Contains(paneFormat, "#{@citadel_managed}") {
		t.Fatal("native probe dropped the ownership marker")
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
