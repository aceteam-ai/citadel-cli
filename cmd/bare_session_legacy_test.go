package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/spf13/cobra"
)

// These exercise the actual bare dispatch seam and the actual Control Center
// contention helper, not a live TUI or transport. An absent/unusable controller
// must not become a prerequisite for reaching the existing monitor-only seam.
func TestBareHeldWorkerRetainsLegacyMonitorSeamWithoutUsableController(t *testing.T) {
	for _, controller := range []string{"no-socket", "permission", "unknown"} {
		t.Run(controller, func(t *testing.T) {
			resetBareDispatchState(t)
			bareStdoutIsTTYFn = func() bool { return true }
			bareStdinIsTTYFn = func() bool { return true }
			bareHasMeshStateFn = func() bool { return true }
			bareHasDeviceCredentialsFn = func() bool { return true }
			bareResolveControlURLFn = func() string { return "https://nexus.fixture.test" }
			bareRunGuidedEnrollFn = func() { t.Fatal("held enrolled worker was sent into enrollment") }
			stateDir := filepath.Join(t.TempDir(), "network")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			owner, err := worklock.Acquire(stateDir, Version, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Release()
			// No listener is started. Unix placeholders deliberately cannot serve
			// status. Windows named-pipe ACLs remain a separate native gate.
			if runtime.GOOS != "windows" && controller != "no-socket" {
				mode := os.FileMode(0o600)
				if controller == "permission" {
					mode = 0
				}
				if err := os.WriteFile(network.LocalAPISocketPath(stateDir), []byte("not a session endpoint"), mode); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			bareRunSessionDispatchStubFn = func(tier enrollmentTier, controlURL string) {
				calls++
				if tier != enrollmentPlatform || controlURL != "https://nexus.fixture.test" {
					t.Fatal("bare command changed its original enrolled dispatch")
				}
				lock, monitor, _, err := acquireControlCenterWorkerLock(stateDir, nil)
				if lock != nil {
					lock.Release()
				}
				if err != nil || !monitor || lock != nil {
					t.Fatal("existing Control Center lock policy lost monitor-only contention")
				}
			}
			runBareCitadel(&cobra.Command{Use: "citadel"}, nil)
			if calls != 1 {
				t.Fatal("bare command dead-ended before the legacy session seam")
			}
		})
	}
}

func TestBareDispatchHasNoViewerOrControllerDiscoveryCalls(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "bare_dispatch.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"enrollmentTierFor": true, "bareHasMeshStateFn": true, "bareHasDeviceCredentialsFn": true,
		"bareCommandActionFor": true, "bareInvocationIsInteractive": true,
		"fmt.Fprintln": true, "cmd.OutOrStdout": true, "bareCommandStatusLine": true,
		"bareRunGuidedEnrollFn": true, "bareRunSessionDispatchStubFn": true, "bareResolveControlURLFn": true,
	}
	found := false
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "runBareCitadel" {
			continue
		}
		found = true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && !allowed[passcodeWiringName(call.Fun)] {
				t.Fatalf("bare dispatcher added discovery/viewer behavior: %s", passcodeWiringName(call.Fun))
			}
			return true
		})
	}
	if !found {
		t.Fatal("actual bare dispatcher missing")
	}
}
