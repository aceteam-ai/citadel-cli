package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hpcProfileSourceFiles are the non-test files that make up the HPC entry
// point. Each must stay free of the surfaces the profile turns off.
var hpcProfileSourceFiles = []string{
	"hpc_work.go",
	"profile.go",
	"hpc_enroll.go",
	"hpc_shutdown.go",
	"hpc_launch.go",
	"../internal/hpc/launch.go",
	"../internal/hpc/venv.go",
	"../internal/hpc/enroll.go",
	"../internal/hpc/labels.go",
	"../internal/hpc/layout.go",
	"../internal/hpcscope/scope.go",
}

// TestHPCEntryPointReferencesNoDisabledSurface is a static tripwire: the HPC
// worker must never import or call a listener, mesh, update, service or
// broad-handler entry point. A new reference here is a design change that
// needs review, not a refactor.
func TestHPCEntryPointReferencesNoDisabledSurface(t *testing.T) {
	forbiddenImports := []string{
		"/internal/terminal", "/internal/gateway", "/internal/egressrelay", "/internal/update",
		"/internal/service", "/internal/ingress", "/internal/reconcile", "/internal/catalog",
		"/internal/jobs", "tailscale.com/",
	}
	forbiddenSelectors := map[string]bool{
		"net.Listen": true, "net.ListenTCP": true, "net.ListenUDP": true, "net.ListenPacket": true,
		"net.ListenUnix": true, "tls.Listen": true, "http.ListenAndServe": true,
		"http.ListenAndServeTLS": true, "http.Serve": true,
		"network.Connect": true, "network.VerifyOrReconnect": true, "network.Listen": true,
		"network.ListenVPN": true, "network.EnsureConnected": true, "network.ConnectMachineWide": true,
		"status.NewServer": true, "worker.NewInferenceQueueReconciler": true,
		"worker.CreateLegacyHandlersWithOpts": true, "worker.NewLivenessMonitor": true,
		"worker.NewAgentUpdateHandler": true, "worker.NewModuleSetHandler": true,
	}
	forbiddenCalls := map[string]bool{
		"resolveWorkerQueues": true, "runWorkerWithAutoUpdater": true, "newReconcileLoop": true,
		"startEgressRelay": true, "startManagedServices": true, "buildNodeJobHandlers": true,
		"registerPrivilegedNodeJobHandlers": true, "newModelSwapManager": true,
		"refreshManagedComposeFiles": true, "checkForUpdateOnStartup": true,
	}
	forbiddenMethods := map[string]bool{"AddQueue": true, "EnableWake": true, "ListenAndServe": true, "WithSwapper": true}

	fset := token.NewFileSet()
	for _, rel := range hpcProfileSourceFiles {
		f, err := parser.ParseFile(fset, filepath.Clean(rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbiddenImports {
				if strings.Contains(path, bad) {
					t.Errorf("%s imports %s", rel, path)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && forbiddenSelectors[id.Name+"."+x.Sel.Name] {
					t.Errorf("%s: references %s.%s", fset.Position(x.Pos()), id.Name, x.Sel.Name)
				}
				if forbiddenMethods[x.Sel.Name] {
					t.Errorf("%s: calls .%s", fset.Position(x.Pos()), x.Sel.Name)
				}
			case *ast.Ident:
				if forbiddenCalls[x.Name] {
					t.Errorf("%s: references %s", fset.Position(x.Pos()), x.Name)
				}
			}
			return true
		})
	}
}

// TestHPCSupervisorListensOnlyOnLoopback pins that the job body's one
// listener (the transient port probe) binds a 127.0.0.1 literal.
func TestHPCSupervisorListensOnlyOnLoopback(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Clean("../internal/hpc/supervise.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Listen" {
			return true
		}
		found++
		lit, ok := call.Args[len(call.Args)-1].(*ast.BasicLit)
		if !ok || !strings.HasPrefix(lit.Value, `"127.0.0.1:`) {
			t.Errorf("%s: Listen must bind a 127.0.0.1 literal", fset.Position(call.Pos()))
		}
		return true
	})
	if found != 1 {
		t.Fatalf("expected exactly one Listen call in supervise.go, found %d", found)
	}
}
