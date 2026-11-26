package laneinventory

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

type fakeProcessReader struct {
	ids           []int32
	err           error
	metadataCalls int
}

func TestInventoryThreadHomeUsesOSAccount(t *testing.T) {
	home := filepath.Join(t.TempDir(), "owner")
	for _, kind := range []string{"valid", "foreign", "relative", "missing", "error"} {
		t.Run(kind, func(t *testing.T) {
			got, err := threadHomeForUID(1000, func(id string) (*user.User, error) {
				if id != "1000" {
					t.Fatal("wrong account requested")
				}
				account := &user.User{Uid: "1000", HomeDir: home}
				switch kind {
				case "foreign":
					account.Uid = "2000"
				case "relative":
					account.HomeDir = "relative"
				case "missing":
					return nil, nil
				case "error":
					return nil, errors.New("private lookup detail")
				}
				return account, nil
			})
			if kind == "valid" {
				if err != nil || got != home {
					t.Fatal("valid account refused")
				}
			} else if got != "" || !errors.Is(err, ErrUnavailable) {
				t.Fatal("uncertain account home selected")
			}
		})
	}
}

func (p *fakeProcessReader) UidsWithContext(context.Context) ([]int32, error) { return p.ids, p.err }
func (p *fakeProcessReader) PpidWithContext(context.Context) (int32, error) {
	p.metadataCalls++
	return 9, nil
}
func (p *fakeProcessReader) CreateTimeWithContext(context.Context) (int64, error) { return 123, nil }
func (p *fakeProcessReader) TerminalWithContext(context.Context) (string, error) {
	return "/pts/7", nil
}

func TestInventoryNativeProcessOwnership(t *testing.T) {
	for _, tc := range []struct {
		ids     []int32
		err     error
		allowed bool
	}{
		{[]int32{1000, 1000}, nil, true},
		{[]int32{2000, 1000}, nil, false},
		{[]int32{1000, 2000}, nil, false},
		{[]int32{2000, 2000}, nil, false},
		{[]int32{1000}, nil, false},
		{nil, nil, false},
		{[]int32{1000, 1000}, errors.New("private observation failure"), false},
	} {
		t.Run(fmt.Sprint(tc.ids, tc.err), func(t *testing.T) {
			p := &fakeProcessReader{ids: tc.ids, err: tc.err}
			out, err := nativeProcesses(context.Background(), 1000, func(context.Context) ([]processRef, error) {
				return []processRef{{pid: 10, reader: p}}, nil
			})
			if err != nil || (len(out) == 1) != tc.allowed || (p.metadataCalls == 1) != tc.allowed {
				t.Fatalf("out=%+v metadata=%d err=%v", out, p.metadataCalls, err)
			}
			if tc.allowed && (out[0].UID != tc.ids[0] || out[0].PID != 10 || out[0].Parent != 9 || out[0].StartedAt != 123) {
				t.Fatal("observed identity changed")
			}
			readCalls := 0
			args, err := nativeArgv(context.Background(), 1000, Process{PID: 10, UID: 1000, StartedAt: 123}, func(ctx context.Context) ([]int32, int64, error) {
				ids, err := p.UidsWithContext(ctx)
				return ids, 123, err
			}, func(context.Context) ([]string, error) {
				readCalls++
				return []string{"codex"}, nil
			})
			if (err == nil) != tc.allowed || (readCalls == 1) != tc.allowed || (len(args) == 1) != tc.allowed {
				t.Fatalf("argv reads=%d args=%v err=%v", readCalls, args, err)
			}
		})
	}
}

func TestInventoryNativeSeams(t *testing.T) {
	// Construction is lazy; never invoke these native functions in source tests.
	c := NewNative("/fixture/not-opened")
	if c.Deps.Panes == nil || c.Deps.Argv == nil || c.Deps.ThreadID == nil || c.Deps.Git == nil || c.Deps.Declarations == nil {
		t.Fatal("native edge missing")
	}
	lookups := []string{}
	env := probeEnv(func(key string) (string, bool) { lookups = append(lookups, key); return "/fixture/" + key, true })
	for _, value := range append(lookups, env...) {
		for _, secret := range []string{"TOKEN", "API_KEY", "GIT_DIR", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT", "SSH_AUTH_SOCK"} {
			if strings.Contains(value, secret) {
				t.Fatalf("unexpected inherited state %q", value)
			}
		}
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_NOSYSTEM=1") || !strings.Contains(joined, "GIT_OPTIONAL_LOCKS=0") || !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("git environment not constrained")
	}
	b := &cappedBuffer{limit: 4}
	if n, err := b.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatal("at-bound write refused")
	}
	if _, err := b.Write([]byte("5")); err == nil || b.Len() != 4 {
		t.Fatal("probe output limit lost")
	}
}

func TestInventoryArgvBirthBinding(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int64
		allowed       bool
	}{
		{"same", 123, 123, true}, {"reused-before", 124, 124, false}, {"reused-during", 123, 124, false}, {"unknown", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observations, reads := 0, 0
			args, err := nativeArgv(context.Background(), 1000, Process{PID: 10, UID: 1000, StartedAt: 123}, func(context.Context) ([]int32, int64, error) {
				observations++
				if observations == 1 {
					return []int32{1000, 1000}, tc.before, nil
				}
				return []int32{1000, 1000}, tc.after, nil
			}, func(context.Context) ([]string, error) {
				reads++
				return []string{"claude", "--resume", threadFixture}, nil
			})
			if (err == nil) != tc.allowed || (len(args) > 0) != tc.allowed {
				t.Fatal("reused process inherited argv identity")
			}
			if tc.before != 123 && reads != 0 {
				t.Fatal("birth mismatch must refuse before argv")
			}
		})
	}
}

func TestInventoryArgvUIDChangesDuringRead(t *testing.T) {
	observations := 0
	args, err := nativeArgv(context.Background(), 1000, Process{PID: 10, UID: 1000, StartedAt: 123}, func(context.Context) ([]int32, int64, error) {
		observations++
		if observations == 1 {
			return []int32{1000, 1000}, 123, nil
		}
		return []int32{1000, 2000}, 123, nil
	}, func(context.Context) ([]string, error) { return []string{"claude", "--resume", threadFixture}, nil })
	if err == nil || len(args) != 0 {
		t.Fatal("argv published after process ownership changed")
	}
}

func TestInventoryNativePanesBeyondDisplayCap(t *testing.T) {
	var raw strings.Builder
	for i := 0; i <= MaxPanes; i++ {
		fmt.Fprintf(&raw, "9|%d|@%d|%%%d|/dev/pts/%d|0\n", 100+i, i, i, i)
	}
	panes, err := readPanes(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "-N" {
			t.Fatal("must not start a tmux server")
		}
		if args[0] == "list-panes" {
			if len(args) != 4 || args[3] != paneFormat {
				t.Fatal("unexpected identity format")
			}
			return []byte(raw.String()), nil
		}
		if len(args) != 5 || args[0] != "display-message" || args[1] != "-p" || args[2] != "-t" {
			t.Fatal("unexpected metadata probe")
		}
		if args[3] == fmt.Sprintf("%%%d", MaxPanes) {
			if args[4] != "#{session_name}" {
				t.Fatal("late pane argv/path metadata requested")
			}
			return []byte("citadel-late\n"), nil
		}
		return []byte("fixture\n"), nil
	}, "")
	if err != nil || len(panes) != MaxPanes+1 || !panes[MaxPanes].Terminal {
		t.Fatal("terminal after display cap was not classified")
	}
	for i := MaxPanes + 1; i <= maxObservedPanes; i++ {
		fmt.Fprintf(&raw, "9|%d|@%d|%%%d|/dev/pts/%d|0\n", 100+i, i, i, i)
	}
	if _, err := parsePanes([]byte(raw.String())); err == nil {
		t.Fatal("complete discovery limit must refuse, not truncate")
	}
}

func TestInventoryNativeAmbiguousPath(t *testing.T) {
	for _, cwd := range []string{"/fixture/\\012", "/fixture/a\nb", "/fixture/a\rb", "/fixture/a\tb"} {
		panes, err := readPanes(context.Background(), func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "list-panes" {
				return []byte("9|10|@1|%2|/dev/pts/7|0\n"), nil
			}
			if args[len(args)-1] == "#{pane_current_path}" {
				return []byte(cwd + "\n"), nil
			}
			return []byte("fixture\n"), nil
		}, "")
		if err != nil || len(panes) != 1 || panes[0].Cwd != "" {
			t.Fatal("ambiguous path must not reach a filesystem reader")
		}
	}
}

func TestInventoryDeclaredIdentity(t *testing.T) {
	expected := declaredsession.ProcessIdentity{PID: 10, StartTicks: 100, UID: 1000, BootID: "boot-a"}
	actual := procscope.ProcessIdentity{PID: 10, StartTicks: 100, UID: 1000, BootID: "boot-a"}
	if !matchesDeclaredIdentity(expected, actual) {
		t.Fatal("matching process not mapped")
	}
	for _, wrong := range []procscope.ProcessIdentity{
		{PID: 11, StartTicks: 100, UID: 1000, BootID: "boot-a"},
		{PID: 10, StartTicks: 101, UID: 1000, BootID: "boot-a"},
		{PID: 10, StartTicks: 100, UID: 2000, BootID: "boot-a"},
		{PID: 10, StartTicks: 100, UID: 1000, BootID: "boot-b"},
	} {
		if matchesDeclaredIdentity(expected, wrong) {
			t.Fatal("stale or foreign declaration mapped")
		}
	}
	if matchesDeclaredIdentity(declaredsession.ProcessIdentity{}, procscope.ProcessIdentity{}) {
		t.Fatal("empty identity mapped")
	}
}

func TestInventoryProcessAndSecretBudgets(t *testing.T) {
	c := fixture()
	c.Deps.Processes = func(context.Context) ([]Process, error) {
		out := []Process{}
		for i := 0; i < MaxProcesses+1; i++ {
			parent := int32(10)
			if i == 0 {
				parent = 9
			}
			out = append(out, Process{PID: int32(i + 10), Parent: parent, UID: 1000, TTY: "/dev/pts/7"})
		}
		return out, nil
	}
	c.Deps.Argv = func(_ context.Context, p Process) ([]string, error) {
		if p.PID == int32(MaxProcesses+10) {
			t.Fatal("process beyond cap observed")
		}
		return []string{"bash"}, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil || !r.Truncated {
		t.Fatal("process cap not reported")
	}
	c = fixture()
	c.Deps.Argv = func(context.Context, Process) ([]string, error) {
		return []string{"bash", "--api-key", strings.Repeat("x", MaxArgBytes-32)}, nil
	}
	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("secret replacement byte budget not enforced")
	}
}
