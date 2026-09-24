//go:build linux

package cmd

import (
	"os/user"
	"strings"
	"testing"
)

func TestNextSubIDRangeAvoidsBothMaps(t *testing.T) {
	uid := []byte("alice:100000:65536\n")
	gid := []byte("bob:165536:65536\n")
	if got := nextSubIDRange(uid, gid); got != 262144 {
		t.Fatalf("next range = %d, want 262144", got)
	}
	if hasSubIDRange(uid, "bob") || !hasSubIDRange(uid, "alice") {
		t.Fatal("subordinate ID ownership was not matched exactly")
	}
	if hasSubIDRange([]byte("alice:100000:1000\n"), "alice") {
		t.Fatal("short mapping must not count as the rootless range")
	}
}

func TestProvisionedUserCommandCarriesRootlessSocket(t *testing.T) {
	owner, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runAsUser(owner.Username, "true").Args, " ")
	for _, required := range []string{
		"XDG_RUNTIME_DIR=/run/user/" + owner.Uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + owner.Uid + "/bus",
		"-u SUDO_USER",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("provision command lacks %q: %s", required, args)
		}
	}
}
