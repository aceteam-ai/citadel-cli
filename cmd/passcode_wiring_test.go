package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

func TestDiagnosticShellUsesStrictStateAndSharedThrottle(t *testing.T) {
	h, ok := jobHandlers["SHELL_COMMAND"].(*jobs.ShellCommandHandler)
	if !ok {
		t.Fatal("diagnostic shell handler missing")
	}
	// Inspect callbacks without calling them: no node state, shell, worker,
	// listener or credential is touched by this wiring assertion.
	if h.State == nil || reflect.ValueOf(h.State).Pointer() != reflect.ValueOf(nodePasscodeState).Pointer() {
		t.Fatal("diagnostic handler must use strict nodePasscodeState")
	}
	if h.Throttle == nil || h.Throttle != nodePasscodeThrottle {
		t.Fatal("diagnostic handler must share the worker process throttle")
	}
	if h.HasPasscode != nil || h.VerifyPasscode != nil {
		t.Fatal("diagnostic handler must not wire the lenient fallback")
	}
}

// Full worker startup probes devices, opens transports and starts services.
// Match the existing workflow_http_test.go pattern: pin source construction and
// the shared legacy bridge without invoking either worker entrypoint.
func TestPasscodeGateBothEntrypointsAndLegacyBridge(t *testing.T) {
	for _, test := range []struct{ file, function, executor string }{
		{"work.go", "runWork", "wfExec"},
		{"controlcenter.go", "runTUIWorker", "ccWfExec"},
		{"nodejobs.go", "buildNodeJobHandlers", ""},
	} {
		t.Run(test.function, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), test.file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, problem := range passcodeWiringErrors(file, test.function, test.executor) {
				t.Error(problem)
			}
		})
	}
}

func passcodeWiringName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			return x.Name + "." + e.Sel.Name
		}
	}
	return ""
}

func passcodeWiringErrors(file *ast.File, function, executor string) []string {
	expected := map[string]map[string]string{
		"jobs.ShellGate": {
			"State":    "nodePasscodeState",
			"Throttle": "nodePasscodeThrottle",
		},
		"nodeJobHandlerOpts": {
			"ShellPasscodeState":   "nodePasscodeState",
			"DesktopPasscodeState": "nodePasscodeState",
			"PasscodeThrottle":     "nodePasscodeThrottle",
			"WorkflowExec":         executor,
		},
	}
	if function == "buildNodeJobHandlers" {
		expected = map[string]map[string]string{
			"worker.LegacyHandlerOpts": {
				"ShellPasscodeState":   "opts.ShellPasscodeState",
				"DesktopPasscodeState": "opts.DesktopPasscodeState",
				"PasscodeThrottle":     "opts.PasscodeThrottle",
			},
		}
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == function {
			body = fn.Body
			break
		}
	}
	if body == nil {
		return []string{"missing function " + function}
	}
	var problems []string
	seen := make(map[string]int)
	ast.Inspect(body, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typ := passcodeWiringName(literal.Type)
		want, relevant := expected[typ]
		if !relevant {
			return true
		}
		seen[typ]++
		fields := make(map[string]string)
		for _, element := range literal.Elts {
			if kv, ok := element.(*ast.KeyValueExpr); ok {
				fields[passcodeWiringName(kv.Key)] = passcodeWiringName(kv.Value)
			}
		}
		for key, value := range want {
			if fields[key] != value {
				problems = append(problems, fmt.Sprintf("%s %s.%s must be %s", function, typ, key, value))
			}
		}
		return true
	})
	for typ := range expected {
		if seen[typ] != 1 {
			problems = append(problems, fmt.Sprintf("%s must inspect exactly one %s, saw %d", function, typ, seen[typ]))
		}
	}
	return problems
}

func TestPasscodeWiringAssertionRejectsMissingOrSplitBindings(t *testing.T) {
	const source = `package fixture
func runWork() {
 _ = jobs.ShellGate{State: nodePasscodeState, Throttle: nodePasscodeThrottle}
 _ = nodeJobHandlerOpts{ShellPasscodeState: nodePasscodeState, DesktopPasscodeState: nodePasscodeState, PasscodeThrottle: nodePasscodeThrottle, WorkflowExec: wfExec}
}`
	parse := func(source string) *ast.File {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	if problems := passcodeWiringErrors(parse(source), "runWork", "wfExec"); len(problems) != 0 {
		t.Fatalf("valid fixture rejected: %v", problems)
	}
	for _, mutation := range []struct{ old, replacement string }{
		{"State: nodePasscodeState", "State: nil"},
		{"Throttle: nodePasscodeThrottle}", "Throttle: jobs.NewPasscodeThrottle()}"},
		{"ShellPasscodeState: nodePasscodeState,", ""},
		{"DesktopPasscodeState: nodePasscodeState", "DesktopPasscodeState: nil"},
		{"PasscodeThrottle: nodePasscodeThrottle", "PasscodeThrottle: jobs.NewPasscodeThrottle()"},
		{"WorkflowExec: wfExec", "WorkflowExec: nil"},
	} {
		mutated := strings.Replace(source, mutation.old, mutation.replacement, 1)
		if mutated == source {
			t.Fatal("mutation failed to change fixture")
		}
		if problems := passcodeWiringErrors(parse(mutated), "runWork", "wfExec"); len(problems) == 0 {
			t.Errorf("accepted broken wiring: %s", mutation.replacement)
		}
	}
}
