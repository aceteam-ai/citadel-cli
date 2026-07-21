// cmd/services_cmd.go
// "citadel services" is the operator view of managed inference engines and their
// usage/idle status (citadel #416). This is distinct from "citadel service"
// (alias "svc"), which manages Citadel itself as a system/systemd service.
package cmd

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/spf13/cobra"
)

var servicesCmd = &cobra.Command{
	Use:   "services",
	Short: "List managed inference engines and their idle/usage status",
	Long: `Show the managed serving engines and catalog apps running on this node,
with a per-service usage signal: whether each is actively serving requests or
sitting idle (holding CPU/RAM/VRAM with no recent activity), how long it has
been idle, and its live resource footprint.

This is the operator-facing view of the idle-detection telemetry that rides the
node heartbeat (citadel #416). It answers "is anyone actually using this engine,
or is it pinning VRAM for nothing?" so you can safely reclaim a contended GPU.

Idle signal sources:
  - vLLM exposes Prometheus request counters, giving a precise last-request /
    idle-seconds signal.
  - Other engines (diffusers, ollama, ...) fall back to a CPU/GPU-utilization
    heuristic: quiet for long enough is reported as idle.
  - A service with no derivable signal is shown as "unknown" and is NEVER a
    candidate for auto-stop.

Auto-stop: OFF by default. Set SERVICE_AUTO_STOP_WHEN_IDLE=true (optionally
SERVICE_AUTO_STOP_IDLE_SECONDS=<n>) on the node so 'citadel work' auto-stops a
service confirmed idle past the threshold. Candidates that WOULD be stopped are
flagged below.`,
	RunE: runServices,
}

func init() {
	rootCmd.AddCommand(servicesCmd)
}

func runServices(_ *cobra.Command, _ []string) error {
	nodeName := ""
	var pinned []string
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		nodeName = manifest.Node.Name
		pinned = manifest.PinnedServices
	}

	collector := status.NewCollector(status.CollectorConfig{
		NodeName:       nodeName,
		ConfigDir:      "",
		Services:       nil,
		PinnedServices: pinned,
	})
	nodeStatus, err := collector.Collect()
	if err != nil {
		return fmt.Errorf("failed to collect node status: %w", err)
	}

	// Build the set of auto-stop candidates for annotation. Using the same
	// confirmed-idle predicate the reconciler uses keeps the displayed "would
	// stop" set exactly consistent with what 'citadel work' would act on. The
	// reconciler is constructed regardless of the enabled gate (with a no-op stop)
	// purely to enumerate candidates for display.
	enabled := status.AutoStopEnabled()
	rec := status.NewAutoStopReconciler(true, status.AutoStopThresholdSeconds(),
		func(status.IdleCandidate) error { return nil }, nil)
	candidates := map[string]bool{}
	for _, c := range rec.Candidates(nodeStatus) {
		candidates[string(c.Kind)+"/"+c.Name] = true
	}

	rows := collectServiceRows(nodeStatus, candidates)
	if len(rows) == 0 {
		fmt.Println("No managed services or apps are running on this node.")
		return nil
	}

	fmt.Printf("Managed services on %s:\n\n", displayNodeName(nodeName))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tSTATUS\tPIN\tUSAGE\tFOOTPRINT\tNOTE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.name, r.kind, r.statusStr, r.pin, r.usage, r.footprint, r.note)
	}
	w.Flush()

	fmt.Printf("\nAuto-stop-when-idle: %s (threshold %ds)\n",
		enabledLabel(enabled), status.AutoStopThresholdSeconds())
	if !enabled {
		fmt.Println("Set SERVICE_AUTO_STOP_WHEN_IDLE=true on the node to let 'citadel work' reclaim idle engines.")
	}
	return nil
}

// serviceRow is one rendered line of the services table.
type serviceRow struct {
	name, kind, statusStr, pin, usage, footprint, note string
}

// collectServiceRows flattens the status services and apps into sorted display
// rows, annotating each with its usage label, footprint, pin state, and an
// eviction-candidate note when it qualifies.
func collectServiceRows(st *status.NodeStatus, candidates map[string]bool) []serviceRow {
	var rows []serviceRow
	add := func(kind, name, statusStr string, pinned bool, idle *status.IdleState, fp *status.ServiceFootprint) {
		note := ""
		if status.IsHeavyAndIdle(fp, idle) {
			note = "heavy+idle"
		}
		if candidates[kind+"/"+name] {
			if note != "" {
				note += ", "
			}
			note += "auto-stop candidate"
		}
		rows = append(rows, serviceRow{
			name:      name,
			kind:      kind,
			statusStr: statusStr,
			pin:       pinLabel(kind, pinned),
			usage:     usageLabel(statusStr, idle),
			footprint: footprintLabel(fp),
			note:      note,
		})
	}
	for i := range st.Services {
		s := &st.Services[i]
		add(string(status.EntityService), s.Name, s.Status, s.Pinned, s.IdleState, s.Footprint)
	}
	for i := range st.Apps {
		a := &st.Apps[i]
		// Apps are not pinnable (pinned_services is a service-level allowlist).
		add(string(status.EntityApp), a.Name, a.Status, false, a.IdleState, a.Footprint)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].kind != rows[j].kind {
			return rows[i].kind < rows[j].kind
		}
		return rows[i].name < rows[j].name
	})
	return rows
}

// pinLabel renders the PIN column (citadel #577): "pinned" for a pinned service,
// "preemptible" for any other service, and "-" for apps (pinning is a
// service-level allowlist, so apps are not pinnable via pinned_services).
func pinLabel(kind string, pinned bool) string {
	if kind == string(status.EntityApp) {
		return "-"
	}
	if pinned {
		return "pinned"
	}
	return "preemptible"
}

// usageLabel renders the usage column: "busy"/"idle <dur>" for a running
// service with a signal, "unknown" when running with no signal, and "-" when not
// running.
func usageLabel(statusStr string, idle *status.IdleState) string {
	if statusStr != status.ServiceStatusRunning {
		return "-"
	}
	if idle == nil {
		return "unknown"
	}
	return status.FormatIdleLabel(idle)
}

// footprintLabel renders the footprint column, "-" when absent.
func footprintLabel(fp *status.ServiceFootprint) string {
	if fp == nil {
		return "-"
	}
	return status.FormatFootprint(fp)
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "ENABLED"
	}
	return "OFF (default)"
}

func displayNodeName(name string) string {
	if name == "" {
		return "this node"
	}
	return name
}
