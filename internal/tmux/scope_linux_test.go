//go:build linux

package tmux

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func withScopeSeams(t *testing.T, cgroup string, lookupErr error) {
	t.Helper()
	origLookup := lookupSystemdRun
	origRead := readSelfCgroup
	origEUID := currentEUID
	lookupSystemdRun = func(string) (string, error) {
		if lookupErr != nil {
			return "", lookupErr
		}
		return "/usr/bin/systemd-run", nil
	}
	readSelfCgroup = func() ([]byte, error) { return []byte(cgroup), nil }
	currentEUID = func() int { return 1001 }
	t.Cleanup(func() {
		lookupSystemdRun = origLookup
		readSelfCgroup = origRead
		currentEUID = origEUID
	})
}

func TestPersistentSessionCommand_UserServiceUsesSiblingScope(t *testing.T) {
	t.Setenv(systemdInvocationIDEnv, "invocation")
	withScopeSeams(t, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/citadel.service\n", nil)

	command := []string{"/usr/bin/tmux", "new-session", "-d", "-s", "agent", "/bin/bash"}
	got := PersistentSessionCommand("agent", command)

	if got[0] != "/usr/bin/systemd-run" {
		t.Fatalf("binary = %q, want systemd-run: %v", got[0], got)
	}
	wantPrefix := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect"}
	if !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("prefix = %v, want %v", got[:len(wantPrefix)], wantPrefix)
	}
	if !strings.HasPrefix(got[len(wantPrefix)], "--unit=citadel-session-agent-") || !strings.HasSuffix(got[len(wantPrefix)], ".scope") {
		t.Errorf("unit argument = %q, want a unique citadel session scope", got[len(wantPrefix)])
	}
	wantTail := append([]string{"--"}, command...)
	if !reflect.DeepEqual(got[len(wantPrefix)+1:], wantTail) {
		t.Errorf("command tail = %v, want %v", got[len(wantPrefix)+1:], wantTail)
	}
	if !reflect.DeepEqual(command, []string{"/usr/bin/tmux", "new-session", "-d", "-s", "agent", "/bin/bash"}) {
		t.Errorf("input command mutated: %v", command)
	}
}

func TestPersistentSessionCommand_ScopesRootSystemService(t *testing.T) {
	t.Setenv(systemdInvocationIDEnv, "invocation")
	withScopeSeams(t, "0::/system.slice/citadel-worker.service\n", nil)
	currentEUID = func() int { return 0 }

	got := PersistentSessionCommand("agent", []string{"tmux", "new-session"})
	if contains(got, "--user") {
		t.Fatalf("system service command unexpectedly targets the user manager: %v", got)
	}
	if !contains(got, "--scope") {
		t.Fatalf("root system service command was not scoped: %v", got)
	}
}

func TestPersistentSessionCommand_NonRootSystemServiceIsUnchanged(t *testing.T) {
	t.Setenv(systemdInvocationIDEnv, "invocation")
	withScopeSeams(t, "0::/system.slice/citadel-worker.service\n", nil)
	command := []string{"tmux", "new-session", "-d", "-s", "agent"}

	if got := PersistentSessionCommand("agent", command); !reflect.DeepEqual(got, command) {
		t.Fatalf("got %v, want unchanged %v", got, command)
	}
}

func TestPersistentSessionCommand_UnmanagedOrUnavailableIsUnchanged(t *testing.T) {
	command := []string{"tmux", "new-session", "-d", "-s", "agent"}

	t.Run("not service managed", func(t *testing.T) {
		t.Setenv(systemdInvocationIDEnv, "")
		withScopeSeams(t, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/citadel.service\n", nil)
		if got := PersistentSessionCommand("agent", command); !reflect.DeepEqual(got, command) {
			t.Fatalf("got %v, want unchanged %v", got, command)
		}
	})

	t.Run("systemd-run missing", func(t *testing.T) {
		t.Setenv(systemdInvocationIDEnv, "invocation")
		withScopeSeams(t, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/citadel.service\n", errors.New("missing"))
		if got := PersistentSessionCommand("agent", command); !reflect.DeepEqual(got, command) {
			t.Fatalf("got %v, want unchanged %v", got, command)
		}
	})
}

func TestPersistentSessionCommand_InvalidSessionNameIsUnchanged(t *testing.T) {
	t.Setenv(systemdInvocationIDEnv, "invocation")
	withScopeSeams(t, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/citadel.service\n", nil)
	command := []string{"tmux", "new-session", "-d", "-s", "bad name"}

	if got := PersistentSessionCommand("bad name", command); !reflect.DeepEqual(got, command) {
		t.Fatalf("got %v, want unchanged %v", got, command)
	}
}

func TestIsUserManagerCgroup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"unified user service", "0::/user.slice/user-1001.slice/user@1001.service/app.slice/citadel.service\n", true},
		{"legacy controller user service", "1:name=systemd:/user.slice/user-1001.slice/user@1001.service/app.slice/citadel.service\n", true},
		{"system service", "0::/system.slice/citadel-worker.service\n", false},
		{"misleading filename", "0::/system.slice/not-user@1001.service-child.scope\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUserManagerCgroup(tc.in); got != tc.want {
				t.Fatalf("isUserManagerCgroup(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
