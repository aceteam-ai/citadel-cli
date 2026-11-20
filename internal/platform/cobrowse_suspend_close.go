package platform

import (
	"context"
	"errors"
	"strconv"
	"time"
)

const suspendGracefulCloseLimit = 5 * time.Second

// A private plan for an already-admitted browser incarnation. exited must be
// that exact root's reaper channel. Neither this plan nor its success grants
// authority or proves release of helpers, display, container, job or ports.
type suspendBrowserClosePlan struct {
	port      int
	browserID string
	exited    <-chan struct{}
}

type suspendCloseConn interface {
	cdpCaller
	Close()
}

type suspendBrowserCloseSource struct {
	endpoint func(context.Context, int) (string, error)
	open     func(context.Context, string) (suspendCloseConn, error)
}

func closeSuspendBrowser(ctx context.Context, plan suspendBrowserClosePlan, fence func() error) error {
	return closeSuspendBrowserWith(ctx, plan, fence, suspendBrowserCloseSource{
		endpoint: fetchCDPBrowserWSContext,
		open: func(ctx context.Context, ws string) (suspendCloseConn, error) {
			c, err := openSuspendCaptureConn(ctx, pin{ws: ws})
			if err != nil {
				return nil, err
			}
			return c, nil
		},
	})
}

// Unwired suspend-only best-effort graceful request plus root-exit observation,
// not ordinary teardown or a complete stop proof. Success does not prove that
// stores flushed or that an already-dead root exited gracefully. The caller
// retains lifecycle exclusion and fences the exact proc/generation/driver
// before and after awaits, permitting the expected root
// exit. Profile/registry effects and positive all-resource proof belong to the
// caller. Injected source operations/connections must honor the supplied
// context. No manager/job invokes this helper. Legacy close remains unchanged.
func closeSuspendBrowserWith(ctx context.Context, plan suspendBrowserClosePlan, fence func() error, source suspendBrowserCloseSource) error {
	fail := func() error { return errors.New("could not confirm browser root exit") }
	if ctx == nil || fence == nil || source.endpoint == nil || source.open == nil ||
		plan.exited == nil || plan.port < 1 || plan.port > 65535 || !validSuspendRestoredTargetID(plan.browserID) {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil {
		return fail()
	}
	// Discovery, dialing, command and reaping share one budget, never a fresh
	// background timeout per operation. A longer caller budget is capped too.
	ctx, cancel := context.WithTimeout(ctx, suspendGracefulCloseLimit)
	defer cancel()
	check := func() bool { return ctx.Err() == nil && fence() == nil && ctx.Err() == nil }
	if !check() {
		return fail()
	}
	select {
	case <-plan.exited:
		if !check() {
			return fail()
		}
		return nil
	default:
	}
	ws, err := source.endpoint(ctx, plan.port)
	if err != nil || !check() || ws != "ws://127.0.0.1:"+strconv.Itoa(plan.port)+"/devtools/browser/"+plan.browserID {
		return fail()
	}
	c, err := source.open(ctx, ws)
	if c != nil {
		defer c.Close()
	}
	if err != nil || c == nil || !check() {
		return fail()
	}
	// Browser.close commonly disconnects without a reply. Neither a successful
	// reply nor a transport error is exit proof: only the pinned reaper is.
	_, _ = c.Call("Browser.close", nil)
	if !check() {
		return fail()
	}
	select {
	case <-plan.exited:
	case <-ctx.Done():
		return fail()
	}
	if !check() {
		return fail()
	}
	return nil
}
