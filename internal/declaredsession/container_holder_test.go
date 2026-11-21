package declaredsession

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func fakeContainerEnv(pid, uid, gid int, cgroup string) containerEnv {
	return containerEnv{
		pid: pid, uid: uid, gid: gid,
		readFile: func(string) ([]byte, error) { return []byte(cgroup), nil },
		hostname: func() (string, error) { return "box", nil },
		stat:     func(string) error { return nil },
	}
}

func TestVerifyContainerSelf(t *testing.T) {
	ok := "0::/docker/abc123\n"
	if c, err := verifyContainerSelf(fakeContainerEnv(1, 1000, 1000, ok)); err != nil || c.identity.Manager != "container" || c.identity.ControlGroup != "0::/docker/abc123" {
		t.Fatalf("valid container = %+v, %v", c, err)
	}
	for name, env := range map[string]containerEnv{
		"not pid 1":                fakeContainerEnv(42, 1000, 1000, ok),
		"root":                     fakeContainerEnv(1, 0, 1000, ok),
		"root group":               fakeContainerEnv(1, 1000, 0, ok),
		"under the worker service": fakeContainerEnv(1, 1000, 1000, "0::/system.slice/citadel-worker.service\n"),
		"empty cgroup":             fakeContainerEnv(1, 1000, 1000, ""),
	} {
		if _, err := verifyContainerSelf(env); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	env := fakeContainerEnv(1, 1000, 1000, ok)
	env.readFile = func(string) ([]byte, error) { return nil, errors.New("denied") }
	if _, err := verifyContainerSelf(env); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreadable cgroup err = %v", err)
	}
}

func TestContainerExitResultMakesSignalsNonZero(t *testing.T) {
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := containerExitResult(live, nil); err != nil {
		t.Errorf("clean harness exit = %v, want nil (exit 0)", err)
	}
	cancel()
	if err := containerExitResult(live, nil); !errors.Is(err, ErrHolderTerminated) {
		t.Errorf("signalled shutdown = %v, want ErrHolderTerminated", err)
	}
	boom := errors.New("storage")
	if err := containerExitResult(live, boom); !errors.Is(err, boom) {
		t.Errorf("holder error must win: %v", err)
	}
}

func TestRunContainerHolderRefusesBadOptionsBeforeCustody(t *testing.T) {
	env := fakeContainerEnv(1, 1000, 1000, "0::/docker/x\n")
	for name, opts := range map[string]ContainerHolderOptions{
		"relative node dir": {NodeDir: "run", Name: "a", Incarnation: "0123456789abcdef0123456789abcdef"},
		"bad name":          {NodeDir: "/run/session", Name: "A b", Incarnation: "0123456789abcdef0123456789abcdef"},
		"bad incarnation":   {NodeDir: "/run/session", Name: "a", Incarnation: "zz"},
	} {
		if err := runContainerHolder(context.Background(), opts, env); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestContainerCustodyCheckMembers(t *testing.T) {
	c := &containerCustody{env: fakeContainerEnv(1, 1000, 1000, "x")}
	if err := c.CheckMembers(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	c.env.stat = func(string) error { return errors.New("gone") }
	if err := c.CheckMembers(context.Background(), []procscopeIdentity{{PID: 1}}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("vanished process err = %v", err)
	}
}

func TestHostDeclarationRevisionUnchangedByContainerFields(t *testing.T) {
	// omitempty on the new fields keeps every existing declaration's revision
	// byte-identical, so an upgraded node sees no config_drift on running sessions.
	d := defaultDeclaration()
	d.Name, d.Harness, d.Command, d.Cwd = "a", "claude-code", []string{"claude"}, "/tmp"
	raw, err := jsonMarshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"runtime"`, `"credentials"`} {
		if contains(raw, key) {
			t.Errorf("host declaration JSON carries %s", key)
		}
	}
}

const containerFixture = `sessions:
  - name: lane1
    harness: claude-code
    command: [claude]
    cwd: /home/agent/work/repo
    runtime: {kind: container, image: ghcr.io/aceteam-ai/session:1}
`

type procscopeIdentity = procscope.ProcessIdentity

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestValidContainerHello(t *testing.T) {
	inc := "0123456789abcdef0123456789abcdef"
	holder := ProcessIdentity{PID: 1, StartTicks: 77, UID: 1000, BootID: "boot"}
	scope := ScopeIdentity{Manager: "container", ControlGroup: "0::/docker/x"}
	saved := State{Name: "a", Incarnation: inc, ProtocolVersion: ProtocolVersion, Holder: holder, Scope: scope, Guarantee: ContainerGuarantee, DeclarationRevision: "sha256:aa", BinaryVersion: "v1"}
	hello := Hello{ProtocolVersion: ProtocolVersion, Name: "a", Incarnation: inc, Holder: holder, Scope: scope, DeclarationRevision: "sha256:aa", BinaryVersion: "v1"}
	peer := ProcessIdentity{PID: 4242, UID: 1000} // host-side view of PID 1
	if !validContainerHello(hello, saved, "a", peer) {
		t.Fatal("matching hello refused")
	}
	for name, mutate := range map[string]func(*Hello, *State, *ProcessIdentity){
		"other incarnation":      func(h *Hello, _ *State, _ *ProcessIdentity) { h.Incarnation = "ffffffffffffffffffffffffffffffff" },
		"holder is not pid 1":    func(h *Hello, s *State, _ *ProcessIdentity) { h.Holder.PID, s.Holder.PID = 9, 9 },
		"peer is another user":   func(_ *Hello, _ *State, p *ProcessIdentity) { p.UID = 1001 },
		"saved is a host holder": func(_ *Hello, s *State, _ *ProcessIdentity) { s.Guarantee = "verified_user_scope" },
		"scope differs":          func(h *Hello, _ *State, _ *ProcessIdentity) { h.Scope.ControlGroup = "0::/docker/y" },
		"revision differs":       func(h *Hello, _ *State, _ *ProcessIdentity) { h.DeclarationRevision = "sha256:bb" },
		"root holder":            func(h *Hello, s *State, p *ProcessIdentity) { h.Holder.UID, s.Holder.UID, p.UID = 0, 0, 0 },
	} {
		h, s, p := hello, saved, peer
		mutate(&h, &s, &p)
		if validContainerHello(h, s, "a", p) {
			t.Errorf("%s accepted", name)
		}
	}
}
