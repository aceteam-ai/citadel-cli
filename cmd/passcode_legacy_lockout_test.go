package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// TestProductionHooksLockLegacyPasscode drives the production hooks this
// package's init installs (#1398): with no master PIN enrolled, the shared
// VerifyPasscode entry point that every network surface calls records failures
// in <node dir>/vault/state.yaml and enforces the vault's lockout.
func TestProductionHooksLockLegacyPasscode(t *testing.T) {
	dir := useWiredLegacyLockout(t)
	perms := config.DefaultPermissions()
	if err := perms.SetPasscode("4821-pass"); err != nil {
		t.Fatal(err)
	}
	if perms.VerifyPasscode("") {
		t.Fatal("empty passcode verified")
	}
	if _, err := os.Stat(filepath.Join(dir, "vault", "state.yaml")); !os.IsNotExist(err) {
		t.Fatalf("an empty passcode must not touch lockout state: %v", err)
	}
	if !perms.VerifyPasscode("4821-pass") {
		t.Fatal("correct passcode rejected")
	}
	for i := 0; i < 5; i++ {
		if perms.VerifyPasscode("wrong") {
			t.Fatal("wrong passcode accepted")
		}
	}
	if perms.VerifyPasscode("4821-pass") {
		t.Fatal("correct passcode accepted during lockout")
	}
	lo := nodevault.OpenLockout(dir).Status()
	if !lo.LockedOut || lo.FailedAttempts != 5 {
		t.Fatalf("lockout state: %+v", lo)
	}
	if nodevault.Open(dir).IsConfigured() {
		t.Fatal("legacy lockout state must not make a master PIN look enrolled")
	}

	if err := resetPasscodeLockoutAt(dir); err != nil {
		t.Fatal(err)
	}
	if !perms.VerifyPasscode("4821-pass") {
		t.Fatal("correct passcode rejected after reset-lockout")
	}
}

// TestLegacyPasscodeVerdictReportsLockoutSeparately: the job path's legacy
// branch uses the same node-wide lockout as the passed-in vault and reports a
// lockout as PasscodeLockedOut, not a mismatch.
func TestLegacyPasscodeVerdictReportsLockoutSeparately(t *testing.T) {
	permsDir, nodeDir := t.TempDir(), t.TempDir()
	p := config.DefaultPermissions()
	if err := p.SetPasscode("4821-pass"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(permsDir, p); err != nil {
		t.Fatal(err)
	}
	st, err := passcodeStateFrom(permsDir, nodevault.Open(nodeDir))
	if err != nil || !st.Set {
		t.Fatalf("legacy state: %+v, %v", st, err)
	}
	if got := st.Check(""); got != jobs.PasscodeMismatch {
		t.Fatalf("empty passcode: %v", got)
	}
	if got := st.Check("4821-pass"); got != jobs.PasscodeMatch {
		t.Fatalf("correct passcode: %v", got)
	}
	for i := 0; i < 5; i++ {
		if got := st.Check("wrong"); got != jobs.PasscodeMismatch {
			t.Fatalf("wrong passcode %d: %v", i, got)
		}
	}
	if got := st.Check("4821-pass"); got != jobs.PasscodeLockedOut {
		t.Fatalf("correct passcode during lockout: %v, want PasscodeLockedOut", got)
	}
	if lo := nodevault.OpenLockout(nodeDir).Status(); lo.FailedAttempts != 5 || !lo.LockedOut {
		t.Fatalf("job checks must share the node-wide lockout: %+v", lo)
	}
}

// TestNetworkPasscodeVerifiersUseVerifyPasscode pins that every network
// passcode verifier cmd wires (status server desktop endpoints, terminal
// servers in both entry points) goes through Permissions.VerifyPasscode, the
// entry point that applies the vault or legacy lockout, and never the raw
// legacy bcrypt check.
func TestNetworkPasscodeVerifiersUseVerifyPasscode(t *testing.T) {
	for _, test := range []struct {
		file string
		want int
	}{{"work.go", 2}, {"controlcenter.go", 1}} {
		file, err := parser.ParseFile(token.NewFileSet(), test.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "PasscodeVerifier" {
				return true
			}
			found++
			calls := map[string]bool{}
			ast.Inspect(assign.Rhs[0], func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if s, ok := call.Fun.(*ast.SelectorExpr); ok {
						calls[s.Sel.Name] = true
					}
				}
				return true
			})
			if !calls["VerifyPasscode"] || calls["VerifyLegacyPasscode"] || calls["CompareHashAndPassword"] {
				t.Errorf("%s: PasscodeVerifier must call Permissions.VerifyPasscode only, calls %v", test.file, calls)
			}
			return true
		})
		if found != test.want {
			t.Errorf("%s: found %d PasscodeVerifier assignments, want %d", test.file, found, test.want)
		}
	}
}
