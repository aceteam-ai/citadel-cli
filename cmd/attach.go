// cmd/attach.go
//
// Discover-and-attach client for the single-instance worker (issues #571 / #524).
//
// `citadel attach` is the docker-daemon-style entry point: instead of launching a
// second citadel process that competes with the running `citadel work` daemon, it
// DISCOVERS the already-running instance and taps into it as an independent,
// read-only client. It names the running node (PID / version / uptime from the
// single-instance lock record) and enriches that with live node identity, health,
// and the managed-service roster read from the daemon's local status server.
//
// Relationship to the rest of #571 / #524:
//   - Phase 0 (stop the duplicate heartbeat) shipped in #574: the control center
//     no longer starts a second heartbeat publisher when a worker holds the lock.
//   - The refusal-path banner (a second `citadel work` prints the running instance
//     instead of a bare error) shipped in #524 increment 1 (cmd/work_attach.go).
//   - This file adds the first-class attach VERB so an operator can inspect the
//     running daemon at any time, not only when they accidentally double-run
//     `citadel work`. It reuses the same lock-record + loopback-status primitives.
//
// Transport is deliberately the UNAUTHENTICATED, loopback-reachable read surface
// (`/status`, `/health`, `/services`) plus the lock record — the same
// zero-daemon-change ethos as work_attach.go. The richer `/agent/*` control
// surface (worker-status, tail logs, restart) is NOT reachable over loopback today
// (it is gated by requireVPNOrAuth, which trusts VPN origin or a token, not
// 127.0.0.1); driving those verbs from a local client is a deferred transport
// question (see the command's deferred-scope note and issue #524). For mutating
// verbs this command points at the existing paths (`citadel logs`, systemctl).
package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/spf13/cobra"
)

// attachCmd implements `citadel attach`.
var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach to the running citadel worker on this node",
	Long: `Discover the citadel worker already running on this node and show its
status, rather than starting a competing second process.

Docker-daemon style: a node runs exactly one worker, and everything else is a
client of it. 'citadel attach' names the running instance (PID, version, uptime)
and shows its live node identity, health, and managed services, read from the
worker's local status server.

If no worker is running, it says so and points you at 'citadel work'.`,
	RunE:         runAttach,
	SilenceUsage: true,
	// The no-worker path prints its own friendly guidance and returns
	// errNoRunningWorker purely to carry a non-zero exit for scripts; silence
	// cobra's own "Error: ..." line so the guidance is not printed twice.
	SilenceErrors: true,
}

// errNoRunningWorker is returned (exit 1) when there is nothing to attach to. The
// human-facing guidance is printed separately; this carries the non-zero exit for
// scripts while keeping the message clean.
var errNoRunningWorker = fmt.Errorf("no citadel worker is running on this node")

func init() {
	rootCmd.AddCommand(attachCmd)
}

// runAttach discovers the running worker via the single-instance lock and prints
// the attach view, or a "nothing to attach to" message when none is running.
func runAttach(_ *cobra.Command, _ []string) error {
	stateDir := network.GetStateDir()

	// IsHeld is the authoritative liveness check (flock probe + PID liveness +
	// reused-PID guard); ReadHolder supplies the recorded identity metadata. A
	// stale lock file (dead holder) reports held=false, so we never attach to a
	// ghost.
	held, _ := worklock.IsHeld(stateDir)
	if !held {
		fmt.Fprint(os.Stdout, noWorkerMessage())
		return errNoRunningWorker
	}

	holder, ok := worklock.ReadHolder(stateDir)
	if !ok {
		// The holder is live (IsHeld) but its lock record was unreadable (e.g. a
		// legacy/garbage record). Present what we can rather than failing.
		holder = worklock.HolderRecord{}
	}

	port := resolveStatusPort()
	var st *attachStatus
	if probed, probeOK := probeLocalStatus(port); probeOK {
		st = &probed
	}
	services, _ := probeLocalServices(port)

	fmt.Fprint(os.Stdout, buildAttachView(holder, time.Now(), st, services))
	return nil
}

// noWorkerMessage is the guidance printed when no worker holds the lock. Pure so
// the copy is unit tested.
func noWorkerMessage() string {
	var b strings.Builder
	b.WriteString("No citadel worker is running on this node.\n\n")
	b.WriteString("Start one with:  citadel work\n")
	return b.String()
}

// attachService is the minimal subset of a status ServiceInfo the attach view
// renders. Kept local (not importing status.ServiceInfo) so the client only
// depends on the handful of fields it prints and tolerates schema drift.
type attachService struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Health string `json:"health"`
	Port   int    `json:"port"`
}

// probeLocalServices fetches the managed-service roster from the loopback status
// server's ungated /services endpoint. Best-effort: any error (no server, wrong
// port, wedged daemon) returns ok=false and the attach view simply omits the
// services section. Reuses the bounded, loopback-only httpGetBody from
// work_attach.go so the probe can never block or reach off-box.
func probeLocalServices(port int) ([]attachService, bool) {
	if port <= 0 {
		return nil, false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	body, ok := httpGetBody(client, fmt.Sprintf("http://127.0.0.1:%d/services", port))
	if !ok {
		return nil, false
	}
	var payload struct {
		Services []attachService `json:"services"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	return payload.Services, true
}

// buildAttachView renders the full attach output: the discover-and-attach banner
// (holder identity + live node/health, reused from work_attach.go) followed by a
// managed-services section when the /services probe succeeded. Pure (no IO) so it
// is unit tested against fixed inputs.
func buildAttachView(holder worklock.HolderRecord, now time.Time, st *attachStatus, services []attachService) string {
	var b strings.Builder
	b.WriteString(buildAttachBanner(holder, now, st))
	if section := renderServicesSection(services); section != "" {
		b.WriteString("\n")
		b.WriteString(section)
	}
	return b.String()
}

// renderServicesSection formats the managed-service roster, sorted by name for a
// stable view. Returns "" when there are no services (nil probe or an empty
// roster) so the attach view omits the header entirely rather than printing an
// empty "Services:" block.
func renderServicesSection(services []attachService) string {
	if len(services) == 0 {
		return ""
	}
	sorted := make([]attachService, len(services))
	copy(sorted, services)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("Managed services:\n")
	for _, s := range sorted {
		name := s.Name
		if name == "" {
			name = "(unnamed)"
		}
		status := s.Status
		if status == "" {
			status = "unknown"
		}
		line := fmt.Sprintf("  %-14s %s", name, status)
		if s.Health != "" {
			line += fmt.Sprintf(" (%s)", s.Health)
		}
		if s.Port > 0 {
			line += fmt.Sprintf(" :%d", s.Port)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
