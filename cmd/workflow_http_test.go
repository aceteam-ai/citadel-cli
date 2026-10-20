package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/workflow"
)

func TestWorkflowHTTPPolicyExplicitOwnedDirectory(t *testing.T) {
	// Never resolve the real node configuration directory in this test.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, workflow.HTTPPolicyFile), []byte("allow: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := workflowHTTPPolicy(dir), workflow.LoadHTTPPolicy(dir); !reflect.DeepEqual(got, want) {
		t.Fatal("wrapper did not preserve local policy")
	}
	if err := os.WriteFile(filepath.Join(dir, workflow.HTTPPolicyFile), []byte("invalid: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := workflowHTTPPolicy(dir)
	if !reflect.DeepEqual(got, workflow.LoadHTTPPolicy(dir)) || reflect.DeepEqual(got, workflow.HTTPPolicy{}) {
		t.Fatal("wrapper discarded policy load failure")
	}
}

func TestWorkflowHTTPPolicyBothEntrypoints(t *testing.T) {
	for _, path := range []string{"work.go", "controlcenter.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		configs := 0
		ast.Inspect(file, func(n ast.Node) bool {
			literal, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typ, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || typ.Sel.Name != "ExecutorConfig" {
				return true
			}
			pkg, ok := typ.X.(*ast.Ident)
			if !ok || pkg.Name != "workflow" {
				return true
			}
			configs++
			wired := false
			for _, field := range literal.Elts {
				kv, ok := field.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "HTTP" {
					continue
				}
				call, ok := kv.Value.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					continue
				}
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || fn.Name != "workflowHTTPPolicy" {
					continue
				}
				arg, ok := call.Args[0].(*ast.CallExpr)
				if !ok || len(arg.Args) != 0 {
					continue
				}
				selector, ok := arg.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "GetNodeConfigDir" {
					continue
				}
				network, ok := selector.X.(*ast.Ident)
				wired = ok && network.Name == "network"
			}
			if !wired {
				t.Errorf("%s: workflow executor lacks shared node-local HTTP policy loader", path)
			}
			return true
		})
		if configs == 0 {
			t.Errorf("%s: executor wiring was not inspected", path)
		}
	}
}
