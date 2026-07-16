// cmd/work_attach.go
//
// Discover-and-attach UX for the single-instance worker (issue #524, increment 1).
//
// When `citadel work` is refused by the single-instance lock (another worker
// already serves this node), an interactive invocation should not dead-end on a
// bare error. Docker-daemon style, it discovers the running instance and prints
// its status instead of duplicating or refusing silently. This file holds that
// increment: the refusal-path attach banner (read-only), rendered from the lock
// record plus an unauthenticated GET /status on the local status server.
//
// Deliberately additive and zero-daemon-change: no new endpoint, no lock
// mutation. The banner is a read-only HTTP client of the already-running worker.
// TTY-gated so systemd / scripts keep today's exit-1 refusal (see decideAttach).
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"golang.org/x/term"
)

// defaultStatusPort mirrors the status server's default bind port (internal/status
// server.go, and the auto-enable in cmd/work.go). Used as the fallback when
// gateway-facts.json is absent or records no status port.
const defaultStatusPort = 8080

// attachStatus is the minimal subset of the /status payload the attach banner
// renders. Kept local (rather than importing status.NodeStatus) so the banner
// only depends on the handful of fields it prints and tolerates schema drift.
type attachStatus struct {
	Version string
	// NodeName is the worker's node identity (status.NodeInfo.Name).
	NodeName string
	// Health is the /health status string ("ok", ...), empty when unknown.
	Health string
}

// decideAttach reports whether to render the friendly discover-and-attach banner
// (true) versus the legacy exit-1 refusal (false). Pure so the TTY/flag policy is
// unit tested without a terminal.
//
//   - --no-attach forces the legacy refusal (scripts that want the old behavior).
//   - --attach forces the banner (e.g. capturing it from a non-TTY).
//   - otherwise the banner is shown only on an interactive TTY. Non-TTY (systemd,
//     journald, pipelines) keeps the exit-1 refusal so a misconfigured double
//     systemd unit still fails visibly instead of "succeeding" as an attach.
func decideAttach(isTTY, attachFlag, noAttachFlag bool) bool {
	if noAttachFlag {
		return false
	}
	if attachFlag {
		return true
	}
	return isTTY
}

// resolveStatusPort returns the plaintext status server port to probe for attach
// info: the port the running gateway persisted in gateway-facts.json, else the
// compile-time default (8080). A daemon started with --no-gateway (no facts file,
// status disabled) degrades attach to lock-record-only info via the default port
// simply failing to connect.
func resolveStatusPort() int {
	if f, ok := readGatewayFacts(); ok && f.StatusPort > 0 {
		return f.StatusPort
	}
	return defaultStatusPort
}

// probeLocalStatus fetches the minimal attach status from the local status server
// over loopback. Best-effort: any error (no server, wrong port, timeout, wedged
// daemon) returns ok=false and the caller falls back to the lock-record-only
// banner. It never blocks `citadel work` for long — a short timeout and a literal
// 127.0.0.1 URL (not "localhost", to dodge IPv6/DNS) keep it snappy.
func probeLocalStatus(port int) (attachStatus, bool) {
	if port <= 0 {
		return attachStatus{}, false
	}
	client := &http.Client{Timeout: 2 * time.Second}

	st, ok := fetchStatusJSON(client, port)
	if !ok {
		return attachStatus{}, false
	}
	// /health is a cheap, separate endpoint; a failure here just leaves Health
	// blank (the /status probe already proved the server is up).
	st.Health = fetchHealth(client, port)
	return st, true
}

// fetchStatusJSON GETs /status and extracts the fields the banner renders.
func fetchStatusJSON(client *http.Client, port int) (attachStatus, bool) {
	body, ok := httpGetBody(client, fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if !ok {
		return attachStatus{}, false
	}
	var payload struct {
		Version string `json:"version"`
		Node    struct {
			Name string `json:"name"`
		} `json:"node"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return attachStatus{}, false
	}
	return attachStatus{Version: payload.Version, NodeName: payload.Node.Name}, true
}

// fetchHealth GETs /health and returns its status string, or "" on any error.
func fetchHealth(client *http.Client, port int) string {
	body, ok := httpGetBody(client, fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if !ok {
		return ""
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Status
}

// httpGetBody performs a bounded GET and returns the body on a 2xx, else ok=false.
func httpGetBody(client *http.Client, url string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	return body, true
}

// buildAttachBanner renders the discover-and-attach banner. Pure (no IO) so it is
// unit tested against a fixed lock record + status payload.
//
// It always renders from the lock record (holder PID, version, and uptime derived
// from the recorded start time — the WORKER's uptime, available even when the
// status probe fails). When st is non-nil it enriches the banner with the live
// node identity and health from /status. The action lines only reference verbs
// that exist today (`citadel logs -f`, systemctl/kill, --no-single-instance);
// richer verbs (`--stop`, `logs --worker`) are later increments.
func buildAttachBanner(holder worklock.HolderRecord, now time.Time, st *attachStatus) string {
	var b strings.Builder
	b.WriteString("Citadel worker already running for this node:\n")

	// Line 1: identity from the lock record (always available).
	version := holder.Version
	if version == "" && st != nil && st.Version != "" {
		version = st.Version
	}
	line := fmt.Sprintf("  PID %d", holder.PID)
	if !holder.StartTime.IsZero() {
		line += fmt.Sprintf(", started %s", holder.StartTime.Format(time.RFC3339))
		line += fmt.Sprintf(", uptime %s", humanizeUptime(now.Sub(holder.StartTime)))
	}
	if version != "" {
		line += fmt.Sprintf(", version %s", version)
	}
	b.WriteString(line + "\n")

	// Line 2: live node identity from /status when the probe succeeded.
	if st != nil && st.NodeName != "" {
		node := "  Node: " + st.NodeName
		if st.Health != "" {
			node += fmt.Sprintf(" (%s)", st.Health)
		}
		b.WriteString(node + "\n")
	}

	b.WriteString("\n")
	b.WriteString("View logs:      citadel logs -f\n")
	b.WriteString("Stop it:        systemctl --user stop citadel  (or kill the PID above)\n")
	b.WriteString("Run a second worker anyway: citadel work --no-single-instance\n")
	return b.String()
}

// humanizeUptime formats a duration as a compact "Nd Nh Nm" (dropping leading
// zero units), e.g. 51h3m -> "2d 3h". Sub-minute durations render as "0m".
func humanizeUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// renderWorkAttach prints the discover-and-attach banner for a refused
// `citadel work`, reading the holder metadata and probing the local status
// server. It writes to stdout (this is informational success for an interactive
// user, not an error) and is only reached after decideAttach approved the banner.
// stateDir is the resolved node state dir the lock is keyed to.
func renderWorkAttach(stateDir string, running *worklock.ErrAlreadyRunning) {
	holder, ok := worklock.ReadHolder(stateDir)
	if !ok {
		// The lock file was unreadable between the refusal and now (holder exited?).
		// Fall back to the error's own PID/start so the banner still names a holder.
		holder = worklock.HolderRecord{PID: running.PID, StartTime: running.StartTime}
	}
	var st *attachStatus
	if probed, probeOK := probeLocalStatus(resolveStatusPort()); probeOK {
		st = &probed
	}
	fmt.Fprint(os.Stdout, buildAttachBanner(holder, time.Now(), st))
}

// stdoutIsTTY reports whether stdout is an interactive terminal (repo precedent:
// internal/tui/styles.go). Systemd / journald / pipes are not TTYs.
func stdoutIsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
