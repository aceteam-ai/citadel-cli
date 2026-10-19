package cmd

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
)

func TestStreamEvalFromRootDoesNotReadNodeConfiguration(t *testing.T) {
	pack, logs := streamCommandFixture(t)
	prior := nodeConfigDirFn
	nodeConfigDirFn = func() string {
		t.Fatal("offline stream eval reached node initialization")
		return ""
	}
	t.Cleanup(func() { nodeConfigDirFn = prior })
	command := &cobra.Command{Use: "citadel", PersistentPreRun: rootCmd.PersistentPreRun}
	command.AddCommand(newStreamCommand())
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"stream", "eval", pack, logs})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
}

// The exception must stay ahead of ALL root initialization, not merely ahead
// of today's first node-config read. The existing no-shadow guard still applies
// to every child, including future live stream commands.
func TestOfflineStreamExceptionIsFirstRootPreRunStatement(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok { t.Fatal("cannot locate root source") }
	source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "root.go"), nil, 0)
	if err != nil { t.Fatal(err) }
	found := false
	ast.Inspect(source, func(n ast.Node) bool {
		field, ok := n.(*ast.KeyValueExpr)
		if !ok { return true }
		name, ok := field.Key.(*ast.Ident)
		if !ok || name.Name != "PersistentPreRun" { return true }
		fn, ok := field.Value.(*ast.FuncLit)
		if !ok || len(fn.Body.List) == 0 { t.Fatal("missing root pre-run body") }
		guard, ok := fn.Body.List[0].(*ast.IfStmt)
		if !ok || guard.Else != nil || len(guard.Body.List) != 1 { t.Fatal("offline exception is not the first narrow guard") }
		condition, ok := guard.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.EQL { t.Fatal("offline exception is not an exact equality") }
		call, ok := condition.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 { t.Fatal("offline exception does not inspect the exact command path") }
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "CommandPath" { t.Fatal("offline exception uses a broad command selector") }
		literal, ok := condition.Y.(*ast.BasicLit)
		if !ok || literal.Value != `"citadel stream eval"` { t.Fatal("offline exception includes a live command") }
		if _, ok := guard.Body.List[0].(*ast.ReturnStmt); !ok { t.Fatal("offline exception does not return immediately") }
		found = true
		return false
	})
	if !found { t.Fatal("root pre-run initializer not found") }
}
