// cmd/agent_tools.go
//
// Building blocks for the agent-facing introspection & control tools (issue
// #236). These back the status server's /agent/* HTTP endpoints, which the
// aceteam MCP server wraps as the citadel_* agent tools. The control path is
// the tsnet status server, deliberately NOT the Redis shell-job queue, so an
// agent can diagnose a node whose job consumption is broken.
package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// agentProviderDeps carries everything buildAgentProviders needs from runWork.
type agentProviderDeps struct {
	state           *worker.WorkerState
	source          worker.JobSource
	nodeName        string
	headscaleNodeID string
	baseURL         string
	deviceConfig    *DeviceConfig
}

// buildAgentProviders constructs the status.AgentProviders that back the
// /agent/* endpoints (issue #236). Closures capture the live worker state and
// source so introspection and control act on the running process, not via the
// Redis job queue.
func buildAgentProviders(ctx context.Context, d agentProviderDeps) *status.AgentProviders {
	orgID := ""
	if d.deviceConfig != nil {
		orgID = d.deviceConfig.OrgID
	}
	startedAt := time.Now()

	return &status.AgentProviders{
		WorkerStatus: func() any {
			return d.state.Snapshot()
		},
		NodeInfo: func() any {
			return agentNodeInfo(d.nodeName, d.headscaleNodeID, orgID, startedAt)
		},
		Doctor: func() any {
			return agentDoctor(d.state.Snapshot())
		},
		Config: func() any {
			return agentConfig(d.nodeName, d.baseURL, orgID, d.state.Snapshot().Queues)
		},
		Logs: func(q status.LogQuery) (string, error) {
			return agentTailLogs(struct {
				Lines int
				Level string
				Grep  string
				Since string
			}{Lines: q.Lines, Level: q.Level, Grep: q.Grep, Since: q.Since})
		},
		SetLogLevel: func(verbose bool) any {
			debugMode = verbose
			Log("agent: log level set verbose=%v", verbose)
			return map[string]any{"ok": true, "verbose": verbose}
		},
		Resubscribe: func() (any, error) {
			return agentResubscribe(ctx, d, orgID)
		},
		WorkerRestart: func() (any, error) {
			// A safe in-place run-loop restart (Drain + reconnect + re-AddQueue)
			// is non-trivial because Runner.Run blocks and owns the source. Rather
			// than ship a half-working goroutine swap, return an actionable result
			// pointing the agent at the supported recovery paths. Tracked as
			// future work in #236.
			return map[string]any{
				"ok":      false,
				"message": "in-place worker restart is not yet supported; use /agent/resubscribe to recover a dead consume loop, or restart the citadel systemd service (sudo systemctl restart citadel)",
			}, nil
		},
	}
}

// agentResubscribe re-establishes the per-node shell stream subscription on the
// live source without restarting the process, to recover from a startup race
// where the Headscale node ID was unresolved (issue #236 / #3914).
func agentResubscribe(ctx context.Context, d agentProviderDeps, orgID string) (any, error) {
	// Re-resolve the Headscale node ID in case it was empty at startup.
	nodeID := d.headscaleNodeID
	if nodeID == "" {
		nodeID = network.GetGlobalNodeID(ctx)
	}
	if nodeID == "" {
		return map[string]any{
			"ok":      false,
			"message": "Headscale node ID still unresolved; cannot subscribe to the per-node stream. Ensure the VPN is connected.",
		}, nil
	}
	if orgID == "" {
		return map[string]any{
			"ok":      false,
			"message": "org id unknown; cannot build the per-node stream name. Re-run 'citadel init'.",
		}, nil
	}

	perNodeQueue := nodeQueueName(orgID, nodeID)
	switch src := d.source.(type) {
	case *worker.APISource:
		src.AddQueue(perNodeQueue)
	case *worker.RedisSource:
		if err := src.AddQueue(ctx, perNodeQueue); err != nil {
			return nil, fmt.Errorf("failed to subscribe to %s: %w", perNodeQueue, err)
		}
	default:
		return map[string]any{"ok": false, "message": "source does not support resubscription"}, nil
	}
	d.state.SetPerNodeQueue(perNodeQueue)
	switch src := d.source.(type) {
	case *worker.APISource:
		d.state.SetQueues(src.QueueNames())
	case *worker.RedisSource:
		d.state.SetQueues(src.QueueNames())
	}
	Log("agent: resubscribed to per-node stream %s", perNodeQueue)
	return map[string]any{
		"ok":             true,
		"per_node_queue": perNodeQueue,
		"message":        "per-node stream subscription re-established",
	}, nil
}

// agentNodeInfo returns node identity for citadel_node_info: headscale + fabric
// node IDs, version, org, tsnet IP, hostname, uptime, connection state.
func agentNodeInfo(nodeName, headscaleNodeID, orgID string, startedAt time.Time) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info := map[string]any{
		"node_name":         nodeName,
		"headscale_node_id": headscaleNodeID,
		"fabric_node_id":    headscaleNodeID, // platform targets nodes by headscale id
		"org_id":            orgID,
		"version":           Version,
		"uptime_seconds":    int64(time.Since(startedAt).Seconds()),
		"connected":         false,
	}
	if hn, err := os.Hostname(); err == nil {
		info["hostname"] = hn
	}
	if st, err := network.GetGlobalStatus(ctx); err == nil {
		info["connected"] = st.Connected
		info["backend_state"] = st.BackendState
		if st.Hostname != "" {
			info["network_hostname"] = st.Hostname
		}
		if st.IPv4 != "" {
			info["tailscale_ip"] = st.IPv4
		}
		if st.NodeID != "" {
			info["headscale_node_id"] = st.NodeID
			info["fabric_node_id"] = st.NodeID
		}
	}
	return info
}

// maxLogLineBytes caps the length of any single log line returned to the caller.
// A line longer than this is truncated with a marker rather than dropped, so one
// pathological line (e.g. a giant single-line JSON blob) can neither abort the
// read nor bloat the HTTP response.
const maxLogLineBytes = 1 << 20 // 1 MiB

// agentTailLogs reads up to opts.Lines lines from ~/.citadel-cli/logs/latest.log,
// applying optional level/grep/since filters. This is where the previously-lost
// worker stdout now lives, because #234 routed the source LogFn and we now wire
// the runner ActivityFn to Log() (issue #236).
func agentTailLogs(opts struct {
	Lines int
	Level string
	Grep  string
	Since string
}) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve home dir: %w", err)
	}
	logPath := filepath.Join(home, ".citadel-cli", "logs", "latest.log")
	f, err := os.Open(logPath)
	if err != nil {
		return "", fmt.Errorf("cannot open log file %s: %w", logPath, err)
	}
	defer f.Close()

	var since time.Time
	if opts.Since != "" {
		if d, derr := time.ParseDuration(opts.Since); derr == nil {
			since = time.Now().Add(-d)
		}
	}

	// Level filtering is best-effort: log lines are "[HH:MM:SS] [CITADEL] msg",
	// and the routed worker lines are prefixed by their level in the message
	// where present. We match the level token case-insensitively in the line.
	levelFilter := strings.ToLower(strings.TrimSpace(opts.Level))
	grep := strings.ToLower(opts.Grep)

	// Read line-by-line with bufio.Reader.ReadString instead of bufio.Scanner.
	// Scanner hard-fails (bufio.Scanner: token too long) on any line exceeding its
	// buffer cap, which aborted the whole read — precisely when a node is
	// misbehaving and emitting long JSON/stacktrace lines (the case an agent most
	// needs to inspect). ReadString has no token-size limit, so a single long line
	// can never cause the log read to 500. We still truncate any individual line
	// to a sane display cap so one pathological line can't bloat the HTTP response.
	reader := bufio.NewReader(f)
	var matched []string
	for {
		line, readErr := reader.ReadString('\n')
		// Process the line before handling the error: ReadString returns the final
		// newline-less line together with io.EOF, so checking the error first would
		// silently drop it.
		if len(line) > 0 {
			// Match previous scanner.Text() semantics (no trailing newline).
			line = strings.TrimRight(line, "\r\n")
			if len(line) > maxLogLineBytes {
				line = line[:maxLogLineBytes] + "…[truncated]"
			}
			lower := strings.ToLower(line)
			keep := true
			if grep != "" && !strings.Contains(lower, grep) {
				keep = false
			}
			if keep && levelFilter != "" && !strings.Contains(lower, levelFilter) {
				keep = false
			}
			if keep && !since.IsZero() && !logLineAfter(line, since) {
				keep = false
			}
			if keep {
				matched = append(matched, line)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", fmt.Errorf("error reading log: %w", readErr)
		}
	}

	// Keep only the last N lines.
	if opts.Lines > 0 && len(matched) > opts.Lines {
		matched = matched[len(matched)-opts.Lines:]
	}
	return strings.Join(matched, "\n"), nil
}

// logLineAfter reports whether a "[HH:MM:SS] ..." log line is at or after the
// given time (today). Lines without a parseable timestamp are kept (returns
// true) so we never silently drop content.
func logLineAfter(line string, since time.Time) bool {
	if !strings.HasPrefix(line, "[") {
		return true
	}
	end := strings.Index(line, "]")
	if end < 0 {
		return true
	}
	ts := line[1:end]
	parsed, err := time.Parse("15:04:05", ts)
	if err != nil {
		return true
	}
	now := time.Now()
	full := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), 0, now.Location())
	return !full.Before(since)
}

// agentDoctor runs the one-shot healthcheck and the "why am I not receiving
// per-node jobs" diagnosis (issue #236). It mirrors the warning branches in
// runWork (Headscale ID empty, org unknown, per-node queue missing, consume
// status != 200) so an agent gets the same conclusions without reading logs.
func agentDoctor(snap worker.WorkerSnapshot) map[string]any {
	checks := []map[string]any{}
	add := func(name string, ok bool, detail string) {
		checks = append(checks, map[string]any{"name": name, "ok": ok, "detail": detail})
	}

	// 1. Network / identity
	netOK := snap.HeadscaleNodeID != ""
	add("headscale_node_id_resolved", netOK, valueOrEmpty(snap.HeadscaleNodeID, "unresolved (node-targeted jobs fall back to shared org stream)"))

	// 2. Org id known
	orgOK := snap.OrgID != ""
	add("org_id_known", orgOK, valueOrEmpty(snap.OrgID, "unknown (per-node stream skipped)"))

	// 3. Per-node subscription present
	perNodeOK := snap.PerNodeQueue != ""
	add("per_node_stream_subscribed", perNodeOK, valueOrEmpty(snap.PerNodeQueue, "NOT subscribed — per-node jobs will be claimed by a peer on the shared stream"))

	// 4. Consuming recently
	add("worker_consuming", snap.Consuming, fmt.Sprintf("last poll: %s", formatTimePtr(snap.LastPollAt)))

	// 5. Consume HTTP status healthy (API mode). 0 = direct redis / unknown.
	consumeOK := snap.LastConsumeStatus == 0 || snap.LastConsumeStatus == 200
	statusDetail := fmt.Sprintf("last consume HTTP status: %d", snap.LastConsumeStatus)
	if snap.LastConsumeError != "" {
		statusDetail += "; last error: " + snap.LastConsumeError
	}
	add("consume_http_status_ok", consumeOK, statusDetail)

	healthy := netOK && orgOK && perNodeOK && snap.Consuming && consumeOK

	diagnosis := "Node looks healthy for per-node job routing."
	switch {
	case !netOK:
		diagnosis = "Headscale node ID is unresolved, so the per-node shell stream was never subscribed. Per-node jobs (terminal_exec, code_*, file reads) fall back to the shared org stream where a peer may claim them. Try /agent/resubscribe or restart the worker after the VPN is fully connected."
	case !orgOK:
		diagnosis = "Org ID is unknown, so the per-node shell stream was skipped. Re-run 'citadel init' to repopulate device config."
	case !perNodeOK:
		diagnosis = "The per-node shell stream is not subscribed even though identity is known. Call /agent/resubscribe to re-establish it."
	case !snap.Consuming:
		diagnosis = "The worker has not completed a poll recently — the consume loop may be stuck. Check logs and consider /agent/worker-restart."
	case !consumeOK:
		diagnosis = fmt.Sprintf("The consume requests are being rejected (HTTP %d). This is the #3924-class failure: the worker is alive but the backend rejects its consume calls. Inspect last_consume_error and the backend.", snap.LastConsumeStatus)
	}

	return map[string]any{
		"healthy":   healthy,
		"checks":    checks,
		"diagnosis": diagnosis,
		"worker":    snap,
	}
}

func valueOrEmpty(v, emptyMsg string) string {
	if v == "" {
		return emptyMsg
	}
	return v
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// agentConfig returns the effective config with secrets redacted (issue #236).
func agentConfig(nodeName, baseURL, orgID string, queues []string) map[string]any {
	home, _ := os.UserHomeDir()
	return map[string]any{
		"node_name":       nodeName,
		"api_base_url":    baseURL,
		"org_id":          orgID,
		"node_config_dir": filepath.Join(home, ".citadel-node"),
		"log_dir":         filepath.Join(home, ".citadel-cli", "logs"),
		"queues":          queues,
		"version":         Version,
		// Secrets (device_api_token, redis password) are intentionally omitted.
	}
}
