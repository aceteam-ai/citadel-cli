package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

func TestMediaRateLimitActualConnectBackoffDiagnostic(t *testing.T) {
	const secret = "synthetic-rate-capability.private"
	rateLimit := &redisapi.RateLimitError{
		StatusCode: 429, Limit: 123, RetryAfter: 17 * time.Second,
		Message: secret, Window: secret, Body: secret,
	}
	original := *rateLimit
	connector := &mockConnector{errs: []error{rateLimit}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs []string
	err := connectWithBackoffLabeledAndLog(ctx, "fixture Redis API", connector, func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
		// The actual loop computes its server-requested wait before logging.
		// Cancellation at that boundary exercises the unchanged ctx-aware
		// sleep path without a real delay, persistent CLI log or goroutine.
		cancel()
	})
	if !errors.Is(err, context.Canceled) || atomic.LoadInt32(&connector.calls) != 1 || len(logs) != 1 ||
		!strings.Contains(logs[0], "limit=123); honoring server backoff of 17s") {
		t.Fatal("actual retry loop changed typed backoff, cancellation or call accounting")
	}
	if strings.Contains(logs[0], secret) || *rateLimit != original || rateLimit.Wait(time.Time{}) != 17*time.Second {
		t.Fatal("actual retry diagnostic leaked or changed original metadata")
	}
}
