// cmd/agent_tools_test.go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestAgentDoctorHealthy(t *testing.T) {
	s := worker.NewWorkerState()
	s.SetIdentity("w", "redis-api", "citadel-workers", "1008", "org-x")
	s.SetQueues([]string{"jobs:v1:shell:org_x"})
	s.SetPerNodeQueue("jobs:v1:shell:org_x:node:1008")
	s.RecordConsumeStatus(200, "")
	s.RecordPoll()

	d := agentDoctor(s.Snapshot())
	if healthy, _ := d["healthy"].(bool); !healthy {
		t.Fatalf("expected healthy node, got %+v", d)
	}
}

func TestAgentDoctorMissingHeadscaleID(t *testing.T) {
	s := worker.NewWorkerState()
	s.SetIdentity("w", "redis-api", "citadel-workers", "", "org-x") // no headscale id
	s.RecordConsumeStatus(200, "")
	s.RecordPoll()

	d := agentDoctor(s.Snapshot())
	if healthy, _ := d["healthy"].(bool); healthy {
		t.Fatalf("expected unhealthy when headscale id missing")
	}
	diag, _ := d["diagnosis"].(string)
	if !strings.Contains(diag, "Headscale node ID") {
		t.Fatalf("diagnosis should mention Headscale node ID, got %q", diag)
	}
}

func TestAgentDoctorConsume400(t *testing.T) {
	// The #3924-class failure: identity is fine, per-node stream subscribed,
	// polling, but consume returns 400. Doctor must surface that.
	s := worker.NewWorkerState()
	s.SetIdentity("w", "redis-api", "citadel-workers", "1008", "org-x")
	s.SetPerNodeQueue("jobs:v1:shell:org_x:node:1008")
	s.RecordConsumeStatus(400, "API error: bad consumer group")
	s.RecordPoll()

	d := agentDoctor(s.Snapshot())
	if healthy, _ := d["healthy"].(bool); healthy {
		t.Fatalf("expected unhealthy on consume 400")
	}
	diag, _ := d["diagnosis"].(string)
	if !strings.Contains(diag, "400") {
		t.Fatalf("diagnosis should mention the 400, got %q", diag)
	}
}

func TestAgentConfigRedacts(t *testing.T) {
	cfg := agentConfig("node-1", "https://aceteam.ai", "org-x", []string{"jobs:v1:shell:org_x"})
	// Should never include secret fields.
	for k := range cfg {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "secret") {
			t.Fatalf("agentConfig leaked a secret field: %s", k)
		}
	}
	if cfg["org_id"] != "org-x" || cfg["api_base_url"] != "https://aceteam.ai" {
		t.Fatalf("agentConfig missing expected fields: %+v", cfg)
	}
}

func TestAgentTailLogsFilters(t *testing.T) {
	// Point HOME at a temp dir with a synthetic log file.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	logDir := filepath.Join(tmp, ".citadel-cli", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format("15:04:05")
	content := strings.Join([]string{
		"[" + now + "] [CITADEL] info: worker started",
		"[" + now + "] [CITADEL] error: consume failed status 400",
		"[" + now + "] [CITADEL] info: subscribed to per-node stream",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logDir, "latest.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := struct {
		Lines int
		Level string
		Grep  string
		Since string
	}{Lines: 100}

	// No filter: all 3 lines.
	out, err := agentTailLogs(opts)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", n, out)
	}

	// Grep filter.
	opts.Grep = "per-node"
	out, _ = agentTailLogs(opts)
	if !strings.Contains(out, "per-node") || strings.Contains(out, "worker started") {
		t.Fatalf("grep filter failed: %q", out)
	}

	// Level filter (error).
	opts.Grep = ""
	opts.Level = "error"
	out, _ = agentTailLogs(opts)
	if !strings.Contains(out, "400") || strings.Contains(out, "worker started") {
		t.Fatalf("level filter failed: %q", out)
	}
}

// TestAgentTailLogsLongLine reproduces the bug where a single log line longer
// than bufio.Scanner's buffer aborted the whole read with "bufio.Scanner: token
// too long", surfacing as `citadel_logs failed: ... 500: error reading log`.
// The reader must return the (truncated) long line without error, and must not
// drop a following final line that has no trailing newline.
func TestAgentTailLogsLongLine(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	logDir := filepath.Join(tmp, ".citadel-cli", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// ~2 MiB single line: exceeds both bufio.Scanner's 64 KiB default and the
	// former 1 MiB Buffer cap, so the old code would have hard-failed here.
	longLine := "[00:00:00] [CITADEL] error: " + strings.Repeat("X", 2<<20)
	// A trailing line WITHOUT a newline verifies ReadString's final-token handling.
	content := longLine + "\n[00:00:01] [CITADEL] info: last line no newline"
	if err := os.WriteFile(filepath.Join(logDir, "latest.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := struct {
		Lines int
		Level string
		Grep  string
		Since string
	}{Lines: 100}

	out, err := agentTailLogs(opts)
	if err != nil {
		t.Fatalf("long line must not error, got: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	// Long line is truncated to the display cap plus marker, not returned whole.
	if !strings.Contains(lines[0], "…[truncated]") {
		t.Fatalf("expected truncation marker on long line")
	}
	if len(lines[0]) > maxLogLineBytes+len("…[truncated]") {
		t.Fatalf("long line not truncated to cap: len=%d", len(lines[0]))
	}
	// Final newline-less line must be preserved (not dropped by EOF handling).
	if lines[1] != "[00:00:01] [CITADEL] info: last line no newline" {
		t.Fatalf("final newline-less line lost or altered: %q", lines[1])
	}
}

func TestAgentNodeInfoFields(t *testing.T) {
	info := agentNodeInfo("node-1", "1008", "org-x", time.Now().Add(-time.Minute))
	if info["node_name"] != "node-1" {
		t.Fatalf("node_name wrong: %+v", info)
	}
	if info["headscale_node_id"] != "1008" || info["fabric_node_id"] != "1008" {
		t.Fatalf("node id fields wrong: %+v", info)
	}
	if up, _ := info["uptime_seconds"].(int64); up < 50 {
		t.Fatalf("uptime should be ~60s, got %d", up)
	}
}
