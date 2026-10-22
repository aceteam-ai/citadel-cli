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

func TestWorkflowLLMPolicyExplicitOwnedDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, workflow.LLMPolicyFile)
	if err := os.WriteFile(path, []byte("allow: [{endpoint: 'http://127.0.0.1:11434/api/chat'}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := workflowLLMPolicy(dir), workflow.LoadLLMPolicy(dir); !reflect.DeepEqual(got, want) {
		t.Fatal("wrapper did not preserve exact machine-local LLM policy")
	}
	if err := os.WriteFile(path, []byte("invalid: true"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := workflowLLMPolicy(dir)
	if !reflect.DeepEqual(got, workflow.LoadLLMPolicy(dir)) || reflect.DeepEqual(got, workflow.LLMPolicy{}) {
		t.Fatal("wrapper discarded fail-closed LLM policy error")
	}
}

func TestWorkflowLLMPolicyBothRealEntrypoints(t *testing.T) {
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
				if !ok || key.Name != "LLM" {
					continue
				}
				call, ok := kv.Value.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					continue
				}
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || fn.Name != "workflowLLMPolicy" {
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
				t.Errorf("%s: real executor lacks machine-local LLM policy wiring", path)
			}
			return true
		})
		if configs == 0 {
			t.Errorf("%s: no actual executor construction inspected", path)
		}
	}
}
