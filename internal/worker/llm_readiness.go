// internal/worker/llm_readiness.go
//
// Per-engine readiness gating for llm_inference (citadel-cli#680).
//
// Before this, serving was gated on residency ("the container is up"), and only
// vllm and sglang had any pre-flight probe at all. unlimited-ocr, llamacpp,
// bonsai and ollama were proxied straight through, so a container that had bound
// its port but was still loading weights surfaced to the caller as a raw
// transport string:
//
//	failed to connect to chat endpoint: Post "http://localhost:8213/v1/chat/completions":
//	... use of closed network connection
//
// On a one-GPU box a cold start is the normal path, not an exception, so that
// window is not a rare error branch: it is the first request of the day, every
// day. A socket error there makes the product look broken rather than busy.
//
// This file adds a readiness probe for EVERY backend and turns "not serving yet"
// into the typed warming answer the platform already understands. Two design
// choices are deliberate and worth reading before changing them:
//
//  1. The probe asks "does the engine's HTTP API answer 200", NOT "does it list
//     a model". llama.cpp and bonsai run in deferred-load / router mode where
//     they answer /v1/models with an EMPTY list and still serve (they load on
//     first request) -- see internal/status/local_engines.go and models.go, which
//     both document that case. Gating on a non-empty model list would make such
//     an engine permanently "warming" and permanently unservable. The stricter
//     model-count predicate stays where it already lives, in the swap manager's
//     post-start wait, where a false negative only means "keep waiting".
//
//  2. The engines that already had a 60s wait keep it; the four that had none get
//     a SHORT probe. Raising every budget above the measured 78s cold start (as
//     the issue first suggested) would exceed the platform's 120s buffered
//     dispatch timeout and convert an honest warming answer into a 504, and it
//     would add a 60s hang to four paths that fail fast today. A short probe plus
//     a typed warming result tells the caller the truth sooner than any wait can.
package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Readiness probe budgets. vllm and sglang keep the 60s wait they already had
// (the vllm/sglang-only waitForReady this replaces) so their behavior does not
// regress; the
// engines that had no probe at all get a short one, because for them the choice
// is between answering "loading, ~Ns" quickly and blocking a request that
// previously failed in milliseconds.
const (
	engineReadyBudgetHealth = 60 * time.Second
	engineReadyBudgetProbe  = 3 * time.Second
	engineReadyPollEvery    = 500 * time.Millisecond
	engineProbeTimeout      = 2 * time.Second
)

// engineReadyPath maps a backend to the endpoint that answers 200 once its API
// is serving. These are the SAME endpoints the heartbeat's model discovery
// already uses (internal/status/models.go), so they are proven against every
// engine this fleet actually runs rather than guessed.
var engineReadyPath = map[string]string{
	"vllm":          "/health",
	"sglang":        "/health",
	"unlimited-ocr": "/v1/models",
	"llamacpp":      "/v1/models",
	"bonsai":        "/v1/models",
	"ollama":        "/api/tags",
}

// engineReadyBudgetOverride shortens the budget in tests. Nil in production.
var engineReadyBudgetOverride *time.Duration

// engineReadyBudget is how long to wait for an engine to start answering before
// reporting it as warming.
func engineReadyBudget(backend string) time.Duration {
	if engineReadyBudgetOverride != nil {
		return *engineReadyBudgetOverride
	}
	switch backend {
	case "vllm", "sglang":
		return engineReadyBudgetHealth
	default:
		return engineReadyBudgetProbe
	}
}

// engineWarmETA is the coarse remaining-load estimate reported to the caller when
// an engine is up but not yet serving. Shares the swap manager's per-engine table
// so one node never quotes two different numbers for the same engine.
func engineWarmETA(backend string) int {
	return int(defaultLoadEstimate(backend).Seconds())
}

// ensureEngineReady polls the backend's readiness endpoint until it answers 200
// or the per-engine budget elapses. A nil error means the engine is serving and
// the request may be proxied. errEngineWarming means it is up but not serving
// yet; the caller must return a typed warming result, never proxy into it.
//
// A backend with no known readiness endpoint is treated as ready (fail-open), so
// adding a backend can never silently make it unservable.
func (h *LLMInferenceHandler) ensureEngineReady(ctx context.Context, backend string) error {
	path, known := engineReadyPath[backend]
	if !known {
		return nil
	}
	baseURL := h.baseURL(backend)
	if baseURL == "" {
		return nil
	}

	deadline := time.Now().Add(engineReadyBudget(backend))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.probeEngine(ctx, baseURL+path) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s is not serving yet", errEngineWarming, backend)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(engineReadyPollEvery):
		}
	}
}

// probeEngine reports whether a single GET returns 200. Bounded by its own short
// timeout so a hung engine cannot consume the whole budget in one attempt.
func (h *LLMInferenceHandler) probeEngine(ctx context.Context, url string) bool {
	pctx, cancel := context.WithTimeout(ctx, engineProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// errEngineWarming marks "the engine is up but not serving yet" so the handler
// can answer with a typed warming result rather than a job failure.
var errEngineWarming = errors.New("engine warming")

// isEngineNotServing classifies an outbound engine request error as "the engine
// was not listening / dropped the connection" rather than a genuine failure.
//
// This is what stops `use of closed network connection` from ever reaching a
// caller: a vLLM that has bound its port but not started serving closes the
// connection mid-write, and the raw string is indistinguishable from a real
// fault. Context cancellation and deadline expiry are deliberately NOT warming:
// those mean the job itself was cancelled or timed out, which is a real failure.
func isEngineNotServing(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := err.Error()
	for _, fragment := range []string{
		"use of closed network connection",
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"server closed idle connection",
		"EOF",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}
