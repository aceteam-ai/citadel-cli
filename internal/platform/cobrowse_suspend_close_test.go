package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type suspendCloseFixture struct {
	plan     suspendBrowserClosePlan
	exited   chan struct{}
	calls    []string
	stale    bool
	rpcError bool
	after    func(string)
}

func newSuspendCloseFixture() *suspendCloseFixture {
	exited := make(chan struct{})
	return &suspendCloseFixture{plan: suspendBrowserClosePlan{port: 9876, browserID: "browser-a", exited: exited}, exited: exited}
}

func (f *suspendCloseFixture) fence() error {
	if f.stale {
		return errors.New("private-close-sentinel")
	}
	return nil
}

func (f *suspendCloseFixture) step(name string) {
	f.calls = append(f.calls, name)
	if f.after != nil {
		f.after(name)
	}
}

func (f *suspendCloseFixture) source(t *testing.T, parent context.Context) suspendBrowserCloseSource {
	t.Helper()
	var budget context.Context
	return suspendBrowserCloseSource{
		endpoint: func(ctx context.Context, port int) (string, error) {
			deadline, finite := ctx.Deadline()
			original, _ := parent.Deadline()
			if !finite || deadline.After(original) || time.Until(deadline) > suspendGracefulCloseLimit || port != 9876 {
				t.Error("graceful root close lost its original bounded budget or port")
			}
			budget = ctx
			f.step("endpoint")
			return "ws://127.0.0.1:9876/devtools/browser/browser-a", nil
		},
		open: func(ctx context.Context, ws string) (suspendCloseConn, error) {
			if ctx != budget || ws != "ws://127.0.0.1:9876/devtools/browser/browser-a" {
				t.Error("graceful root close changed its budget or endpoint")
			}
			f.step("open")
			return f, nil
		},
	}
}

func (f *suspendCloseFixture) Call(method string, params map[string]any) (map[string]any, error) {
	if method != "Browser.close" || len(params) != 0 {
		return nil, errors.New("unexpected private close command")
	}
	f.step("call")
	if f.rpcError {
		return nil, errors.New("private-close-sentinel")
	}
	return map[string]any{}, nil
}

func (f *suspendCloseFixture) Close() { f.step("close") }

func TestSuspendGracefulCloseRequiresPinnedReaper(t *testing.T) {
	for _, rpcError := range []bool{false, true} {
		f := newSuspendCloseFixture()
		f.rpcError = rpcError
		f.after = func(phase string) {
			if phase == "call" {
				close(f.exited)
			}
		}
		ctx := suspendPagesContext(t)
		if err := closeSuspendBrowserWith(ctx, f.plan, f.fence, f.source(t, ctx)); err != nil {
			t.Fatal("the exact reaper was not observed after the close request", err)
		}
		if !reflect.DeepEqual(f.calls, []string{"endpoint", "open", "call", "close"}) {
			t.Fatal("graceful close skipped a stage or leaked the connection")
		}
		f.calls = nil
		if err := closeSuspendBrowserWith(ctx, f.plan, f.fence, f.source(t, ctx)); err != nil || len(f.calls) != 0 {
			t.Fatal("already-reaped retry touched a debugger endpoint")
		}
	}
}

func TestSuspendGracefulCloseAdmissionBeforeIO(t *testing.T) {
	for _, kind := range []string{"nil-context", "no-budget", "cancelled", "nil-fence", "stale", "nil-endpoint", "nil-open", "no-reaper", "port-zero", "port-large", "empty-id", "path-id", "encoded-id", "long-id"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendCloseFixture()
			ctx, fence := suspendPagesContext(t), f.fence
			source := f.source(t, ctx)
			switch kind {
			case "nil-context":
				ctx = nil
			case "no-budget":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-fence":
				fence = nil
			case "stale":
				f.stale = true
			case "nil-endpoint":
				source.endpoint = nil
			case "nil-open":
				source.open = nil
			case "no-reaper":
				f.plan.exited = nil
			case "port-zero":
				f.plan.port = 0
			case "port-large":
				f.plan.port = 65536
			case "empty-id":
				f.plan.browserID = ""
			case "path-id":
				f.plan.browserID = "other/browser-a"
			case "encoded-id":
				f.plan.browserID = "browser%2Fa"
			case "long-id":
				f.plan.browserID = strings.Repeat("x", 129)
			}
			if err := closeSuspendBrowserWith(ctx, f.plan, fence, source); err == nil || len(f.calls) != 0 {
				t.Fatal("invalid graceful-close plan crossed an I/O boundary")
			}
		})
	}
}

func TestSuspendGracefulCloseFencesEachAwait(t *testing.T) {
	for _, phase := range []string{"endpoint", "open", "call", "reaped"} {
		t.Run(phase, func(t *testing.T) {
			f := newSuspendCloseFixture()
			ctx := suspendPagesContext(t)
			f.after = func(at string) {
				if at == "call" {
					close(f.exited)
				}
				if at == phase {
					f.stale = true
				}
			}
			checks := 0
			fence := func() error {
				checks++
				if phase == "reaped" && checks == 5 {
					f.stale = true
				}
				return f.fence()
			}
			if err := closeSuspendBrowserWith(ctx, f.plan, fence, f.source(t, ctx)); err == nil || strings.Contains(err.Error(), "private-close-sentinel") {
				t.Fatal("changed incarnation was acknowledged or provider detail leaked")
			}
			want := map[string][]string{
				"endpoint": {"endpoint"}, "open": {"endpoint", "open", "close"},
				"call": {"endpoint", "open", "call", "close"}, "reaped": {"endpoint", "open", "call", "close"},
			}[phase]
			if !reflect.DeepEqual(f.calls, want) {
				t.Fatal("graceful close continued after a stale boundary or leaked its connection", f.calls)
			}
		})
	}
}

func TestSuspendGracefulCloseProviderRefusals(t *testing.T) {
	for _, kind := range []string{"endpoint-error", "wrong-browser", "wrong-port", "query", "fragment", "userinfo", "open-error", "open-error-with-connection", "nil-connection"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendCloseFixture()
			ctx := suspendPagesContext(t)
			// If a broken guard reaches the command, finish immediately so
			// refusal assertions, not a timeout, explain the regression.
			f.after = func(phase string) {
				if phase == "call" {
					close(f.exited)
				}
			}
			source := f.source(t, ctx)
			endpoint, open := source.endpoint, source.open
			source.endpoint = func(ctx context.Context, port int) (string, error) {
				ws, err := endpoint(ctx, port)
				switch kind {
				case "endpoint-error":
					err = errors.New("private-close-sentinel")
				case "wrong-browser":
					ws += "-other"
				case "wrong-port":
					ws = strings.Replace(ws, ":9876/", ":9877/", 1)
				case "query":
					ws += "?q"
				case "fragment":
					ws += "#part"
				case "userinfo":
					ws = strings.Replace(ws, "ws://", "ws://user@", 1)
				}
				return ws, err
			}
			source.open = func(ctx context.Context, ws string) (suspendCloseConn, error) {
				if kind == "nil-connection" || kind == "open-error" {
					f.step("open")
					if kind == "open-error" {
						return nil, errors.New("private-close-sentinel")
					}
					return nil, nil
				}
				c, err := open(ctx, ws)
				if kind == "open-error-with-connection" {
					err = errors.New("private-close-sentinel")
				}
				return c, err
			}
			if err := closeSuspendBrowserWith(ctx, f.plan, f.fence, source); err == nil || strings.Contains(err.Error(), "private-close-sentinel") {
				t.Fatal("invalid provider result acknowledged or leaked")
			}
			if strings.Contains(strings.Join(f.calls, ","), "call") {
				t.Fatal("invalid provider result reached Browser.close")
			}
			closed := kind == "open-error-with-connection"
			if strings.Contains(strings.Join(f.calls, ","), "close") != closed {
				t.Fatal("refused connection was not closed exactly when owned")
			}
		})
	}
}

func TestSuspendGracefulCloseWaitsWithinOneBudget(t *testing.T) {
	for _, rpcError := range []bool{false, true} {
		f := newSuspendCloseFixture()
		f.rpcError = rpcError
		ctx, cancel := context.WithCancel(suspendPagesContext(t))
		defer cancel()
		called := make(chan struct{})
		f.after = func(phase string) {
			if phase == "call" {
				close(called)
			}
		}
		done := make(chan error, 1)
		go func() { done <- closeSuspendBrowserWith(ctx, f.plan, f.fence, f.source(t, ctx)) }()
		select {
		case <-called:
		case <-ctx.Done():
			t.Fatal("graceful close never reached its command")
		}
		select {
		case <-done:
			t.Fatal("RPC result substituted for positive root reaping")
		default:
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("unreaped root acknowledged after cancellation")
			}
		case <-time.After(time.Second):
			t.Fatal("original context did not bound reaper wait")
		}
		if !reflect.DeepEqual(f.calls, []string{"endpoint", "open", "call", "close"}) {
			t.Fatal("cancelled close leaked its connection")
		}
	}
}

func TestSuspendGracefulCloseLongBudgetIsCapped(t *testing.T) {
	f := newSuspendCloseFixture()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	f.after = func(phase string) {
		if phase == "call" {
			close(f.exited)
		}
	}
	if err := closeSuspendBrowserWith(ctx, f.plan, f.fence, f.source(t, ctx)); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendGracefulCloseCancellationAtEachAwait(t *testing.T) {
	for _, phase := range []string{"endpoint", "open", "call"} {
		t.Run(phase, func(t *testing.T) {
			f := newSuspendCloseFixture()
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			f.after = func(at string) {
				if at == "call" {
					close(f.exited)
				}
				if at == phase {
					cancel()
				}
			}
			if closeSuspendBrowserWith(ctx, f.plan, f.fence, f.source(t, ctx)) == nil {
				t.Fatal("cancelled provider success was acknowledged")
			}
			want := map[string][]string{
				"endpoint": {"endpoint"}, "open": {"endpoint", "open", "close"}, "call": {"endpoint", "open", "call", "close"},
			}[phase]
			if !reflect.DeepEqual(f.calls, want) {
				t.Fatal("cancelled close continued I/O or leaked its connection", f.calls)
			}
		})
	}
}

func TestSuspendGracefulCloseReapedRetryStillFenced(t *testing.T) {
	f := newSuspendCloseFixture()
	close(f.exited)
	ctx := suspendPagesContext(t)
	checks := 0
	fence := func() error {
		checks++
		if checks == 2 {
			f.stale = true
		}
		return f.fence()
	}
	if closeSuspendBrowserWith(ctx, f.plan, fence, f.source(t, ctx)) == nil || len(f.calls) != 0 {
		t.Fatal("already-reaped retry lost its final local fence")
	}
}

func TestSuspendGracefulCloseFenceCannotCancelAndAcknowledge(t *testing.T) {
	for _, alreadyReaped := range []bool{false, true} {
		f := newSuspendCloseFixture()
		ctx, cancel := context.WithCancel(suspendPagesContext(t))
		defer cancel()
		if alreadyReaped {
			close(f.exited)
		}
		checks := 0
		fence := func() error {
			checks++
			if (!alreadyReaped && checks == 1) || (alreadyReaped && checks == 2) {
				cancel()
			}
			return nil
		}
		if closeSuspendBrowserWith(ctx, f.plan, fence, f.source(t, ctx)) == nil || len(f.calls) != 0 {
			t.Fatal("local fence cancellation acknowledged or allowed later I/O")
		}
	}
}
