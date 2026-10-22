package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/tui/sessionview"
	"github.com/spf13/cobra"
)

const validSessionView = `{"mode":"worker","connected":true,"node_name":"fixture","mesh_ip":"100.64.0.1","version":"test","pid":123,"uptime_seconds":10}`

type sessionViewBody struct {
	io.Reader
	closed bool
}

func (b *sessionViewBody) Close() error { b.closed = true; return nil }

func TestSessionViewerLoadsOnlyBoundedExistingStatus(t *testing.T) {
	body := &sessionViewBody{Reader: strings.NewReader(validSessionView)}
	calls := 0
	status, err := loadSessionViewStatus(context.Background(), "fixture-state", func(ctx context.Context, stateDir string) (*http.Response, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second || stateDir != "fixture-state" {
			t.Fatal("status request lost its local destination or timeout")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	})
	if err != nil || calls != 1 || !body.closed || status.PID != 123 || status.Mode != "worker" || !status.Connected {
		t.Fatal("existing status was not projected and closed exactly once")
	}
	// The serialized response bound counts all framing, including whitespace.
	bounded := validSessionView + strings.Repeat(" ", sessionViewBodyLimit-len(validSessionView))
	if _, err := decodeSessionViewStatus([]byte(bounded)); err != nil {
		t.Fatal("exact response bound was refused")
	}
	if _, err := decodeSessionViewStatus([]byte(bounded + " ")); err != errSessionViewUnavailable {
		t.Fatal("one byte beyond response bound was accepted")
	}
}

func TestSessionViewerRejectsMalformedOrUnavailableStatus(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{`, validSessionView + `{}`, validSessionView + `secret`,
		strings.Replace(validSessionView, `"pid":123`, `"pid":null`, 1),
		strings.Replace(validSessionView, `"pid":123`, `"pid":0`, 1),
		strings.Replace(validSessionView, `"pid":123`, `"pid":4294967296`, 1),
		strings.Replace(validSessionView, `"pid":123`, `"pid":1.5`, 1),
		strings.Replace(validSessionView, `"pid":123`, `"pid":123,"pid":456`, 1),
		strings.Replace(validSessionView, `"connected":true,`, ``, 1),
		strings.Replace(validSessionView, `"connected":true`, `"connected":null`, 1),
		strings.Replace(validSessionView, `"uptime_seconds":10`, `"uptime_seconds":-1`, 1),
		strings.Replace(validSessionView, `"uptime_seconds":10`, `"uptime_seconds":9223372036854775808`, 1),
		strings.Replace(validSessionView, `"worker"`, `"unexpected"`, 1),
		strings.Repeat("x", sessionViewBodyLimit+1), "\xff",
	} {
		body := &sessionViewBody{Reader: strings.NewReader(raw)}
		status, err := loadSessionViewStatus(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		})
		if err != errSessionViewUnavailable || !body.closed || status.PID != 0 || status.Connected {
			t.Fatal("invalid status escaped its static refusal or retained old owner fields")
		}
	}
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusFound, http.StatusServiceUnavailable} {
		body := &sessionViewBody{Reader: strings.NewReader("private-peer-error")}
		_, err := loadSessionViewStatus(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: body}, nil
		})
		if err != errSessionViewUnavailable || !body.closed || strings.Contains(err.Error(), "private-peer-error") {
			t.Fatal("HTTP refusal retained peer text or leaked a response")
		}
	}
	for _, response := range []*http.Response{nil, {StatusCode: http.StatusOK}} {
		_, err := loadSessionViewStatus(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) { return response, nil })
		if err != errSessionViewUnavailable {
			t.Fatal("missing status response/body was accepted")
		}
	}
	_, err := loadSessionViewStatus(context.Background(), "fixture", func(context.Context, string) (*http.Response, error) {
		return nil, errors.New("private transport error")
	})
	if err != errSessionViewUnavailable {
		t.Fatal("transport error was not projected to a static refusal")
	}
}

func TestSessionViewerStatusAllowsOptionalFutureFields(t *testing.T) {
	status, err := decodeSessionViewStatus([]byte(strings.TrimSuffix(validSessionView, "}") + `,"future":{"extra":true}}`))
	if err != nil || status.PID != 123 {
		t.Fatal("optional future status fields broke the existing projection")
	}
}

func TestSessionViewerRunnerPropagatesTerminalFailureAndBoundsRequest(t *testing.T) {
	var requestContext context.Context
	calls := 0
	err := runSessionAttach(context.Background(), "fixture-state", func(ctx context.Context, stateDir string) (*http.Response, error) {
		calls++
		requestContext = ctx
		if stateDir != "fixture-state" {
			t.Fatal("viewer changed the resolved local destination")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(validSessionView))}, nil
	}, func(_ context.Context, model *sessionview.Model) error {
		command := model.Init()
		model.Update(command()) // No terminal/program, goroutine, or listener.
		return errors.New("synthetic terminal initialization failure")
	})
	if err == nil || calls != 1 || requestContext.Err() != context.Canceled {
		t.Fatal("terminal failure was lost or the completed request retained its context")
	}
}

func TestObservedSessionRunnerRetainsPreflightTargetAndBaseline(t *testing.T) {
	baseline, err := decodeSessionViewStatus([]byte(validSessionView))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = runObservedSessionAttach(context.Background(), "fixed-observed-state", baseline, func(_ context.Context, stateDir string) (*http.Response, error) {
		calls++
		if stateDir != "fixed-observed-state" {
			t.Fatal("viewer resolved a different destination after preflight")
		}
		changed := strings.Replace(validSessionView, `"pid":123`, `"pid":456`, 1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(changed))}, nil
	}, func(_ context.Context, model *sessionview.Model) error {
		if !strings.Contains(model.View(), "PID: 123") {
			t.Fatal("runner did not seed the validated observation")
		}
		model.Update(model.Init()())
		if !strings.Contains(model.View(), "unavailable or changed") || strings.Contains(model.View(), "PID: 456") {
			t.Fatal("runner silently adopted a different first-poll owner")
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatal("observed viewer did not use exactly its fixed-target first poll")
	}
}

func TestSessionAttachRootHookSkipsSharedStartup(t *testing.T) {
	t.Setenv("CITADEL_AUTH_HOST", "")
	originalConfig := deviceConfigForCATrustFn
	originalUpdate := checkForUpdateOnStartupFn
	t.Cleanup(func() { deviceConfigForCATrustFn = originalConfig; checkForUpdateOnStartupFn = originalUpdate })
	deviceConfigForCATrustFn = func() *DeviceConfig { t.Fatal("viewer read device credentials during CA bootstrap"); return nil }
	checkForUpdateOnStartupFn = func(bool) { t.Fatal("viewer started update/cache work") }
	root := &cobra.Command{Use: "citadel"}
	session := &cobra.Command{Use: "session"}
	attach := &cobra.Command{Use: "attach"}
	root.AddCommand(session)
	session.AddCommand(attach)
	rootCmd.PersistentPreRun(attach, nil)

	// Also pin the ordering against startup writes that have no injected hook:
	// the existing offline guard stays first, followed immediately by this
	// viewer's separate exact-leaf guard, both returning before startup calls.
	source, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "root.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		field, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		name, ok := field.Key.(*ast.Ident)
		if !ok || name.Name != "PersistentPreRun" {
			return true
		}
		function, ok := field.Value.(*ast.FuncLit)
		if !ok || len(function.Body.List) < 2 {
			t.Fatal("shared startup hook changed shape")
		}
		guards := make([]*ast.IfStmt, 2)
		for index, literal := range []string{`"citadel stream eval"`, `"citadel session attach"`} {
			guard, ok := function.Body.List[index].(*ast.IfStmt)
			if !ok || guard.Init != nil || guard.Else != nil || len(guard.Body.List) != 1 {
				t.Fatal("offline and viewer exemptions must be the first two narrow guards")
			}
			if _, ok := guard.Body.List[0].(*ast.ReturnStmt); !ok {
				t.Fatal("exact-leaf exemption does not return before startup work")
			}
			condition, ok := guard.Cond.(*ast.BinaryExpr)
			if !ok || condition.Op != token.EQL {
				t.Fatal("each exemption must retain its separate exact equality")
			}
			value, ok := condition.Y.(*ast.BasicLit)
			if !ok || value.Value != literal {
				t.Fatal("offline and viewer exemptions changed order or exact leaf")
			}
			guards[index] = guard
		}
		for path, want := range map[string]bool{
			"citadel stream eval": true, "citadel session attach": true,
			"citadel": false, "citadel attach": false, "citadel session status": false,
			"citadel session stop": false, "citadel stream serve": false,
			"citadel session attach-extra": false, "other session attach": false,
		} {
			first := evalSessionStartupGuard(t, guards[0].Cond, path)
			second := evalSessionStartupGuard(t, guards[1].Cond, path)
			if (first || second) != want {
				t.Fatal("startup exemption changed its exact command-leaf scope")
			}
		}
		if len(function.Body.List) != 4 {
			t.Fatal("root startup must contain only the two exact guards, bare deferral and shared startup")
		}
		bareGuard, ok := function.Body.List[2].(*ast.IfStmt)
		if !ok || bareGuard.Init != nil || bareGuard.Else != nil || len(bareGuard.Body.List) != 1 {
			t.Fatal("bare discovery must defer startup before any shared mutator")
		}
		if _, ok := bareGuard.Body.List[0].(*ast.ReturnStmt); !ok {
			t.Fatal("bare discovery guard did not return before mutating startup")
		}
		assertInvocationCall(t, bareGuard.Cond, "deferBareStartup")
		startup, ok := function.Body.List[3].(*ast.ExprStmt)
		if !ok {
			t.Fatal("shared startup is no longer the last root hook statement")
		}
		assertInvocationCall(t, startup.X, "prepareAgentInvocation")
		found = true
		return false
	})
	if !found {
		t.Fatal("shared startup hook was not audited")
	}
}

func assertInvocationCall(t *testing.T, expression ast.Expr, name string) {
	t.Helper()
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		t.Fatal("root invocation boundary call changed shape")
	}
	function, ok := call.Fun.(*ast.Ident)
	if !ok || function.Name != name {
		t.Fatal("root invocation boundary called a different helper")
	}
	for index, expected := range []string{"cmd", "args"} {
		argument, ok := call.Args[index].(*ast.Ident)
		if !ok || argument.Name != expected {
			t.Fatal("root invocation boundary lost the current command or arguments")
		}
	}
}

// Evaluate only the actual narrow predicate's allowed AST forms. This refuses
// `if true`, broad prefix tests or a different receiver without invoking any
// neighboring command's real CA/bootstrap, repair, logging or updater hooks.
func evalSessionStartupGuard(t *testing.T, condition ast.Expr, path string) bool {
	t.Helper()
	expression, ok := condition.(*ast.BinaryExpr)
	if !ok {
		t.Fatal("startup exemption is not an explicit command-path predicate")
	}
	if expression.Op != token.EQL {
		t.Fatal("startup exemption no longer uses exact command equality")
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		t.Fatal("startup exemption command-path call changed")
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "CommandPath" {
		t.Fatal("startup exemption no longer reads the command path")
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "cmd" {
		t.Fatal("startup exemption no longer reads this command")
	}
	literal, ok := expression.Y.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		t.Fatal("startup exemption no longer compares a literal command")
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		t.Fatal(err)
	}
	if value != "citadel stream eval" && value != "citadel session attach" {
		t.Fatal("startup exemption contains an unapproved command literal")
	}
	return path == value
}
