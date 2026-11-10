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
	// Selectors are keyed by the RESOLVED import path (so an aliased import
	// cannot hide one) and the member name.
	forbiddenSelectors := map[string]bool{
		"net.Listen": true, "net.ListenTCP": true, "net.ListenUDP": true, "net.ListenPacket": true,
		"net.ListenUnix": true, "net.ListenUnixgram": true, "net.ListenIP": true,
		"crypto/tls.Listen": true, "net/http.ListenAndServe": true,
		"net/http.ListenAndServeTLS": true, "net/http.Serve": true, "net/http.ServeTLS": true,
		"internal/network.Connect": true, "internal/network.VerifyOrReconnect": true, "internal/network.Listen": true,
		"internal/network.ListenVPN": true, "internal/network.EnsureConnected": true, "internal/network.ConnectMachineWide": true,
		"internal/status.NewServer": true, "internal/worker.NewInferenceQueueReconciler": true,
		"internal/worker.CreateLegacyHandlersWithOpts": true, "internal/worker.NewLivenessMonitor": true,
		"internal/worker.NewAgentUpdateHandler": true, "internal/worker.NewModuleSetHandler": true,
	}
	forbiddenCalls := map[string]bool{
		"resolveWorkerQueues": true, "runWorkerWithAutoUpdater": true, "newReconcileLoop": true,
		"startEgressRelay": true, "startManagedServices": true, "buildNodeJobHandlers": true,
		"registerPrivilegedNodeJobHandlers": true, "newModelSwapManager": true,
		"refreshManagedComposeFiles": true, "checkForUpdateOnStartup": true,
	}
	forbiddenMethods := map[string]bool{"AddQueue": true, "EnableWake": true, "ListenAndServe": true, "WithSwapper": true}

	fset := token.NewFileSet()
	check := func(rel string, f *ast.File, root ast.Node) {
		aliases := map[string]string{}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = path
		}
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					if path, ok := aliases[id.Name]; ok {
						for key := range forbiddenSelectors {
							dot := strings.LastIndex(key, ".")
							pkg, member := key[:dot], key[dot+1:]
							if member == x.Sel.Name && (path == pkg || strings.HasSuffix(path, "/"+pkg)) {
								t.Errorf("%s: references %s.%s", fset.Position(x.Pos()), path, x.Sel.Name)
							}
						}
					}
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
		check(rel, f, f)
	}

	// The HPC path also calls helpers that live in other cmd files. Follow
	// each by name and hold its body to the same rules, resolving that
	// file's own import aliases.
	followed := map[string]bool{
		"startStatusPublisherAfterRunner": false, "newWorkSessionProviders": false,
		"enableWebSocketWithRetry": false, "createUsagePublishFn": false,
		"connectWithBackoff": false, "connectWithBackoffLabeled": false,
		"getDeviceConfigFromFile": false,
	}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range matches {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, rel, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if _, want := followed[fd.Name.Name]; want {
				followed[fd.Name.Name] = true
				check(rel, f, fd)
			}
		}
	}
	for name, found := range followed {
		if !found {
			t.Errorf("tripwire could not find helper %s; update the followed list", name)
		}
	}
}
