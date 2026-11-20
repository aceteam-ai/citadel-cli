package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Check startup wiring without running a worker, browser or service.
func TestWorkStartsSuspendHousekeepingFromRootContext(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "work.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var work *ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runWork" {
			work = fn
		}
	}
	if work == nil {
		t.Fatal("worker entry point unavailable")
	}
	var managedStart, maintenance token.Pos
	starts, launches := 0, 0
	rootContext, deferredCancel := false, false
	ast.Inspect(work.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 2 || len(node.Rhs) != 1 {
				break
			}
			ctx, a := node.Lhs[0].(*ast.Ident)
			cancel, b := node.Lhs[1].(*ast.Ident)
			call, c := node.Rhs[0].(*ast.CallExpr)
			if !a || !b || !c || ctx.Name != "ctx" || cancel.Name != "cancel" {
				break
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}
			pkg, ok := selector.X.(*ast.Ident)
			rootContext = ok && pkg.Name == "context" && selector.Sel.Name == "WithCancel"
		case *ast.DeferStmt:
			if fn, ok := node.Call.Fun.(*ast.Ident); ok && fn.Name == "cancel" {
				deferredCancel = true
			}
		case *ast.CallExpr:
			if selector, ok := node.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "StartManagedCobrowse" {
				starts++
				managedStart = node.Pos()
			}
		case *ast.GoStmt:
			selector, ok := node.Call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "RunSuspendHousekeeping" {
				break
			}
			launches++
			maintenance = node.Pos()
			if len(node.Call.Args) != 1 {
				t.Fatal("housekeeping lost the worker context")
			}
			ctx, ok := node.Call.Args[0].(*ast.Ident)
			if !ok || ctx.Name != "ctx" {
				t.Fatal("housekeeping uses an independent lifetime")
			}
			getter, ok := selector.X.(*ast.CallExpr)
			if !ok {
				t.Fatal("housekeeping is not tied to the managed singleton")
			}
			get, ok := getter.Fun.(*ast.SelectorExpr)
			if !ok || get.Sel.Name != "GetCobrowseSessionManager" {
				t.Fatal("housekeeping uses another manager")
			}
		}
		return true
	})
	if starts != 1 || launches != 1 || maintenance <= managedStart || !rootContext || !deferredCancel {
		t.Fatal("managed recovery and lifetime-bound housekeeping startup changed")
	}
}
