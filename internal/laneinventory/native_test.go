package laneinventory

import (
	"context"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func TestInventoryNativeSeams(t *testing.T) {
	// Construction is lazy; never invoke these native functions in source tests.
	c := NewNative("/fixture/not-opened")
	if c.Deps.Panes == nil || c.Deps.Argv == nil || c.Deps.Git == nil || c.Deps.Declarations == nil {
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
			out = append(out, Process{PID: int32(i + 10), Parent: 10, UID: 1000})
		}
		return out, nil
	}
	c.Deps.Argv = func(_ context.Context, pid int32) ([]string, error) {
		if pid == int32(MaxProcesses+10) {
			t.Fatal("process beyond cap observed")
		}
		return []string{"bash"}, nil
	}
	r, err := c.Collect(context.Background())
	if err != nil || !r.Truncated {
		t.Fatal("process cap not reported")
	}
	c = fixture()
	c.Deps.Argv = func(context.Context, int32) ([]string, error) {
		return []string{"bash", "--api-key", strings.Repeat("x", MaxArgBytes-32)}, nil
	}
	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("secret replacement byte budget not enforced")
	}
}
