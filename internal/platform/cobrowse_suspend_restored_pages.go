package platform

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

// targetIDs maps manifest order to the new browser's already-admitted targets.
// This is a private observation plan, not authority to launch/navigate/select
// pages. The manager must hold lifecycle exclusion and supply an incarnation
// fence covering its exact runtime, generation and driver state.
type suspendRestoredPagePlan struct {
	manifest  suspendManifest
	port      int
	browserID string
	targetIDs []string
}

type suspendRestoredPageSource struct {
	verify   func(context.Context, int, string) error
	list     func(context.Context, int) ([]cdpdiscovery.Target, error)
	position func(context.Context, pin, suspendTab, func() error) error
}

func verifySuspendRestoredPages(ctx context.Context, plan suspendRestoredPagePlan, active func() (string, error), fence func() error) error {
	base := browserSuspendPageSource()
	return verifySuspendRestoredPagesWith(ctx, plan, active, fence, suspendRestoredPageSource{
		verify: base.verify, list: base.list,
		position: func(ctx context.Context, p pin, tab suspendTab, fence func() error) error {
			c, err := openSuspendCaptureConn(ctx, p)
			if err != nil {
				return err
			}
			defer c.Close()
			return verifySuspendTabScrollOn(ctx, c, tab, fence)
		},
	})
}

// Unwired bounded final observations for reopen completion. Discovery must be
// complete, not the truncated status projection. Every page is checked without
// scrolling it; extra/missing/navigated targets refuse. Observations are
// sequential, not an atomic page snapshot or a promise of future layout
// stability. Navigation/load stabilization remains the manager's obligation.
// active and fence must be bounded local reads, never browser/remote awaits;
// source operations must cooperate with the unchanged caller-owned context.
func verifySuspendRestoredPagesWith(ctx context.Context, plan suspendRestoredPagePlan, active func() (string, error), fence func() error, source suspendRestoredPageSource) error {
	fail := func() error { return errors.New("could not confirm restored browser pages") }
	if ctx == nil || ctx.Err() != nil || active == nil || fence == nil ||
		source.verify == nil || source.list == nil || source.position == nil {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite || plan.port < 1 || plan.port > 65535 ||
		!validSuspendRestoredTargetID(plan.browserID) ||
		validateSuspendManifest(plan.manifest, plan.manifest.SessionID, plan.manifest.Generation) != nil ||
		len(plan.targetIDs) != len(plan.manifest.Tabs) {
		return fail()
	}
	encoded, err := encodeSuspendManifest(plan.manifest)
	clear(encoded)
	if err != nil {
		return fail()
	}
	// Copy the bounded plan before any callback can mutate the caller's slices.
	plan.manifest.Tabs = append([]suspendTab(nil), plan.manifest.Tabs...)
	plan.targetIDs = append([]string(nil), plan.targetIDs...)
	byID := make(map[string]int, len(plan.targetIDs))
	activeID := ""
	endpoint := func(id string) string { return "ws://127.0.0.1:" + strconv.Itoa(plan.port) + "/devtools/page/" + id }
	for i, id := range plan.targetIDs {
		if !validSuspendRestoredTargetID(id) {
			return fail()
		}
		if _, duplicate := byID[id]; duplicate {
			return fail()
		}
		byID[id] = i
		if plan.manifest.Tabs[i].Active {
			activeID = id
		}
	}
	check := func() error {
		if ctx.Err() != nil || fence() != nil {
			return fail()
		}
		id, err := active()
		if err != nil || id != activeID || ctx.Err() != nil || fence() != nil {
			return fail()
		}
		return nil
	}
	verify := func() bool {
		return check() == nil && source.verify(ctx, plan.port, plan.browserID) == nil && check() == nil
	}
	list := func() bool {
		if check() != nil {
			return false
		}
		targets, err := source.list(ctx, plan.port)
		if err != nil || check() != nil || len(targets) != len(plan.targetIDs) {
			return false
		}
		seen := make(map[string]bool, len(targets))
		for _, target := range targets {
			i, found := byID[target.ID]
			if !found || seen[target.ID] || target.Type != "page" ||
				target.WebSocketDebuggerURL != endpoint(target.ID) || target.URL != plan.manifest.Tabs[i].URL {
				return false
			}
			seen[target.ID] = true
		}
		return true
	}
	if !verify() || !list() {
		return fail()
	}
	for i, id := range plan.targetIDs {
		if !verify() || check() != nil ||
			source.position(ctx, pin{background: true, port: plan.port, id: id, ws: endpoint(id)}, plan.manifest.Tabs[i], check) != nil || check() != nil {
			return fail()
		}
	}
	if !list() || !verify() {
		return fail()
	}
	return nil
}

func validSuspendRestoredTargetID(id string) bool {
	return id != "" && len(id) <= 128 && id != "." && id != ".." && url.PathEscape(id) == id
}
