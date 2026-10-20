package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

func TestPasscodeStateFromFailsClosedOnUnreadableState(t *testing.T) {
	// No permissions file and no vault: no passcode set.
	permsDir := t.TempDir()
	st, err := passcodeStateFrom(permsDir, nodevault.Open(t.TempDir()))
	if err != nil || st.Set {
		t.Fatalf("fresh node: state = %+v, %v; want unset, nil", st, err)
	}

	// A legacy passcode hash is reported as set and verifies.
	p := config.DefaultPermissions()
	if err := p.SetPasscode("4821-pass"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(permsDir, p); err != nil {
		t.Fatal(err)
	}
	st, err = passcodeStateFrom(permsDir, nodevault.Open(t.TempDir()))
	if err != nil || !st.Set || !st.Verify("4821-pass") || st.Verify("wrong") {
		t.Fatalf("legacy passcode: state = %+v, %v", st, err)
	}

	// An unparseable permissions file is an error, never "no passcode".
	badPerms := t.TempDir()
	if err := os.WriteFile(filepath.Join(badPerms, "permissions.yaml"), []byte("passcode_hash: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := passcodeStateFrom(badPerms, nodevault.Open(t.TempDir())); err == nil {
		t.Fatal("an unparseable permissions file must be an error")
	}

	// A vault that cannot be stat'ed is an error, never "no master PIN".
	notADir := filepath.Join(t.TempDir(), "node-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := passcodeStateFrom(t.TempDir(), nodevault.Open(notADir)); err == nil {
		t.Fatal("an unreadable vault must be an error")
	}
}
