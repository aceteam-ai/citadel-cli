// cmd/ps_cmd.go
//
// "citadel ps" is the live, per-workload resource inventory for THIS node: one
// row per running citadel-managed workload with its RAM / VRAM / CPU, the way
// "docker ps" and "ollama ps" list running units (citadel-cli#1127). It answers
// "what is actually holding this node's RAM/GPU right now", which "citadel
// status" (node vitals + GPU total + on-disk model cache) does not attribute to
// individual workloads.
//
// It is READ-ONLY: one status collection, format, print. It never starts,
// stops, or evicts anything (that is "citadel services"/"citadel service"
// territory, and this command is a deliberately distinct sibling that only
// reports).
//
// It does NOT reinvent docker-stats / nvidia-smi / proc parsing. The managed
// rows come from the same status.Collector the heartbeat and "citadel services"
// use (Services + Apps, each already carrying a live ServiceFootprint with
// RAM/VRAM/CPU attributed per workload). The optional --system rows come from
// internal/resmon, the same per-pid GPU-consumer snapshot "citadel status"
// prints under RESOURCE CONSUMERS.
//
// The row-building and formatting below are pure functions over an injected
// *status.NodeStatus / *resmon.Snapshot so they are unit-testable without a GPU,
// a container runtime, or the live node (ps_cmd_test.go). Manifest resolution
// honors --node-dir for free: runPs reads it through findAndReadManifest, the
// single --node-dir-aware choke point (see cmd/nodedir.go).
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/resmon"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/spf13/cobra"
)

var (
	psSystem bool   // --system: also list non-citadel GPU holders
	psJSON   bool   // --json: machine-readable output
	psSortBy string // --sort: vram | ram | cpu
)

// psSystemTimeout bounds the resmon snapshot collection for --system. Matches
// the 6s bound printResourcesInfo uses for the same snapshot in cmd/status.go.
var psSystemTimeout = 6 * time.Second

var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "Live per-workload resource inventory (RAM/VRAM/CPU) for this node",
	Long: `Show the running workloads on this node and their live resource use, one row
per workload, the way 'docker ps' and 'ollama ps' list running units.

Unlike 'citadel status' (node vitals, GPU total, on-disk model cache), this
attributes RAM, VRAM, and CPU to each individual running service or app, so when
a node is memory or GPU pressured you can see what is actually holding it.

Default scope is citadel-managed workloads (running serving engines and catalog
apps). Pass --system to ALSO list the GPU holders citadel did not launch (a
model started outside citadel, a build, a dev process), each marked KIND=system,
so a contended GPU's real owner is visible in one table.

It is read-only: it collects status once, formats, and prints. It never starts,
stops, or evicts anything.

The VRAM column is the differentiator over 'docker ps': it is the per-workload
GPU memory attributed from nvidia-smi compute-apps. On a node with no GPU (or no
nvidia-smi) it renders as "-" and the command still works.`,
	Example: `  # Running citadel-managed workloads, default sort (VRAM then RAM)
  citadel ps

  # Also show non-citadel GPU holders (a model started outside citadel, a build)
  citadel ps --system

  # Machine-readable
  citadel ps --json

  # Sort by RAM (or cpu)
  citadel ps --sort ram`,
	RunE: runPs,
}

func init() {
	rootCmd.AddCommand(psCmd)
	psCmd.Flags().BoolVar(&psSystem, "system", false, "Also list non-citadel GPU/RAM holders on the node (KIND=system)")
	psCmd.Flags().BoolVar(&psJSON, "json", false, "Output in JSON format")
	psCmd.Flags().StringVar(&psSortBy, "sort", "vram", "Sort workloads by: vram, ram, or cpu")
	// --no-color is a global persistent flag on rootCmd (handled in cmd/root.go),
	// so "citadel ps --no-color" works without being re-declared here.
}

// psOptions carries the resolved, validated flag state into the pure builder.
type psOptions struct {
	system bool
	sortBy string // "vram" | "ram" | "cpu" (validated by runPs)
}

// psRow is one rendered workload row. CPU/RAM/VRAM are pointers so a genuinely
// unknown value (no stats row, no GPU) is distinguishable from a real zero and
// renders as "-" rather than a misleading "0". The struct is also the --json
// element; pointer fields are omitempty so an unknown value is simply absent.
type psRow struct {
	// Name is the workload identity: a managed service/app name, or the resmon
	// owner string ("container:<name>" / "host:<comm>") for a system row.
	Name string `json:"name"`
	// Kind is "service", "app" (the status collector's EntityKind values), or
	// "system" (a non-citadel GPU holder from resmon).
	Kind string `json:"kind"`
	// Runtime is the engine dialect for a serving engine (vllm/ollama/llamacpp/
	// tei/...), else the container runtime (docker/podman) when a container stats
	// row resolved, else "container"/"host" for a system row. Empty ("-") when
	// not determinable.
	Runtime string `json:"runtime,omitempty"`
	// Status is the workload status ("running"). System rows are always running
	// (they are live compute processes).
	Status string `json:"status"`
	// CPUPercent is container CPU% (can exceed 100 on multi-core). nil = unknown.
	CPUPercent *float64 `json:"cpu_percent,omitempty"`
	// RAMBytes is resident memory (RSS). nil = unknown.
	RAMBytes *uint64 `json:"ram_bytes,omitempty"`
	// VRAMBytes is per-workload GPU memory, summed across GPUs. nil = no GPU
	// signal (renders "-"); a real 0 on a GPU node renders "0.0G".
	VRAMBytes *uint64 `json:"vram_bytes,omitempty"`
	// Port is the host port the workload binds, 0 (omitted / "-") when none.
	Port int `json:"port,omitempty"`
	// PID is the host process id, populated for system rows (from nvidia-smi);
	// 0 (omitted / "-") for managed rows, which the collector does not expose.
	PID int `json:"pid,omitempty"`
	// Reclaimable flags a heavy, idle, UNMANAGED system holder worth freeing.
	Reclaimable bool `json:"reclaimable,omitempty"`
	// Note is a short justification (e.g. the reclaimable reason), empty otherwise.
	Note string `json:"note,omitempty"`
}

// psJSONOutput is the --json envelope, mirroring "citadel status --json" in
// carrying the node name alongside the rows.
type psJSONOutput struct {
	Node      string  `json:"node,omitempty"`
	Workloads []psRow `json:"workloads"`
}

func runPs(_ *cobra.Command, _ []string) error {
	switch psSortBy {
	case "vram", "ram", "cpu":
	default:
		return fmt.Errorf("invalid --sort %q: want one of vram, ram, cpu", psSortBy)
	}

	// Manifest is --node-dir-aware via findAndReadManifest. A node with no
	// manifest still has a running-engine view (the collector detects running
	// engines directly), so a missing manifest is non-fatal here, matching
	// runServices.
	nodeName := ""
	var pinned []string
	var managedNames []string
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		nodeName = manifest.Node.Name
		pinned = manifest.PinnedServices
		for _, s := range manifest.Services {
			if s.Name != "" {
				managedNames = append(managedNames, s.Name)
			}
		}
	}

	// Same collector path as "citadel services"/the heartbeat: Services:nil lets
	// the collector detect running engines directly, ConfigDir:"" keeps hotswap
	// off (we want running workloads only, not installed-but-stopped ones).
	collector := status.NewCollector(status.CollectorConfig{
		NodeName:            nodeName,
		PinnedServices:      pinned,
		PermissionsProvider: loadNodePermissions,
	})
	nodeStatus, err := collector.Collect()
	if err != nil {
		return fmt.Errorf("failed to collect node status: %w", err)
	}

	// --system adds the non-citadel GPU holders from the same resmon snapshot
	// "citadel status" prints. managedNames classifies bare-named managed
	// services as managed so they are not double-listed as system rows.
	var snap *resmon.Snapshot
	if psSystem {
		ctx, cancel := context.WithTimeout(context.Background(), psSystemTimeout)
		defer cancel()
		s := resmon.CollectWithManaged(ctx, managedNames)
		snap = &s
	}

	runtimeBin := catalog.SelectContainerRuntime().EngineBin

	rows := buildPSRows(nodeStatus, snap, runtimeBin, psOptions{system: psSystem, sortBy: psSortBy})

	if psJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(psJSONOutput{Node: nodeName, Workloads: rows})
	}

	if len(rows) == 0 {
		fmt.Println("No running workloads on this node.")
		return nil
	}

	fmt.Printf("Workloads on %s:\n\n", displayNodeName(nodeName))
	formatPSTable(os.Stdout, rows)
	if !psSystem {
		fmt.Println("\nPass --system to also show non-citadel GPU/RAM holders on this node.")
	}
	return nil
}

// buildPSRows is the pure core: it flattens a collected node status (managed
// services + apps) and, when --system is set, a resmon snapshot (non-citadel GPU
// holders) into sorted display rows. It performs no I/O, so it is unit-tested
// over injected fixtures with no GPU / container runtime / live node. runtimeBin
// is the detected container runtime ("docker"/"podman", possibly "").
func buildPSRows(st *status.NodeStatus, snap *resmon.Snapshot, runtimeBin string, opts psOptions) []psRow {
	// Non-nil empty slice so --json emits [] rather than null on an empty node.
	rows := make([]psRow, 0)

	if st != nil {
		for i := range st.Services {
			s := &st.Services[i]
			if s.Status != status.ServiceStatusRunning {
				continue // ps lists RUNNING workloads only (docker-ps semantics)
			}
			rows = append(rows, psServiceRow(s, runtimeBin))
		}
		for i := range st.Apps {
			a := &st.Apps[i]
			if a.Status != status.ServiceStatusRunning {
				continue
			}
			rows = append(rows, psAppRow(a, runtimeBin))
		}
	}

	if opts.system && snap != nil {
		for i := range snap.Consumers {
			c := &snap.Consumers[i]
			// Managed consumers are already listed above from the collector;
			// skip them here so a GPU-resident managed service is not shown
			// twice. resmon classifies managed by the "citadel-" prefix OR a
			// manifest name, and catalog apps use the "citadel-app-" prefix, so
			// both managed services and apps fall into OwnerCitadelManaged.
			if c.Kind == resmon.OwnerCitadelManaged {
				continue
			}
			rows = append(rows, psSystemRow(c))
		}
	}

	psSortRows(rows, opts.sortBy)
	return rows
}

// psServiceRow builds a row for a running managed service.
func psServiceRow(s *status.ServiceInfo, runtimeBin string) psRow {
	row := psRow{
		Name:    s.Name,
		Kind:    string(status.EntityService),
		Status:  s.Status,
		Runtime: psServiceRuntime(s.Name, runtimeBin, s.Footprint != nil),
		Port:    s.Port,
	}
	psApplyFootprint(&row, s.Footprint)
	return row
}

// psAppRow builds a row for a running catalog app.
func psAppRow(a *status.AppInfo, runtimeBin string) psRow {
	row := psRow{
		Name:   a.Name,
		Kind:   string(status.EntityApp),
		Status: a.Status,
		Port:   a.Port,
	}
	// An app is a container, so its runtime is the container runtime -- but only
	// claim it when a stats row actually resolved (Footprint != nil); otherwise
	// the runtime is honestly unknown.
	if a.Footprint != nil {
		row.Runtime = runtimeBin
	}
	psApplyFootprint(&row, a.Footprint)
	return row
}

// psSystemRow builds a row for a non-citadel GPU holder from a resmon snapshot.
func psSystemRow(c *resmon.Consumer) psRow {
	row := psRow{
		Name:        c.Owner,
		Kind:        "system",
		Runtime:     string(c.Kind), // "container" | "host"
		Status:      status.ServiceStatusRunning,
		PID:         c.PID,
		Reclaimable: c.Reclaimable,
		Note:        c.Reason,
	}
	// resmon does not sample per-process CPU in v1 (CPUPercent is always -1), so
	// the CPU column is honestly "-" for system rows.
	if c.CPUPercent >= 0 {
		cpu := c.CPUPercent
		row.CPUPercent = &cpu
	}
	if c.RSSBytes > 0 {
		ram := c.RSSBytes
		row.RAMBytes = &ram
	}
	// A system row comes from the nvidia-smi compute-apps list, so it is a GPU
	// process on a node with a GPU: VRAM is always a real value (even 0).
	vram := c.VRAMBytes
	row.VRAMBytes = &vram
	return row
}

// psServiceRuntime resolves the RUNTIME column for a managed service: the engine
// dialect for a known serving engine (vllm/ollama/llamacpp/tei/...), else the
// container runtime when a stats row actually resolved, else "" ("-"). It never
// guesses "docker" from a nil footprint, so a host-systemd ollama or an adopted
// external vLLM (which report running with no container stats) is not falsely
// labeled as containerized.
func psServiceRuntime(name, runtimeBin string, hasFootprint bool) string {
	if eng := status.EngineTypeFromName(name); eng != "" {
		return eng
	}
	if hasFootprint {
		return runtimeBin
	}
	return ""
}

// psApplyFootprint copies the live footprint's CPU/RAM/VRAM onto a row, honoring
// each field's documented unknown-vs-zero contract (ServiceFootprint): CPU -1 is
// unknown, RAM 0 is unknown, and VRAM is only meaningful when the node has a GPU
// (HasGPU) -- there a 0 is a real zero. A nil footprint leaves all three unknown.
func psApplyFootprint(row *psRow, fp *status.ServiceFootprint) {
	if fp == nil {
		return
	}
	if fp.CPUPercent >= 0 {
		cpu := fp.CPUPercent
		row.CPUPercent = &cpu
	}
	if fp.RAMBytes > 0 {
		ram := fp.RAMBytes
		row.RAMBytes = &ram
	}
	if fp.HasGPU {
		vram := fp.VRAMBytes
		row.VRAMBytes = &vram
	}
}

// psSortRows orders rows in place. The default "vram" sorts by VRAM descending
// then RAM descending (the scarce resources on a GPU node); "ram"/"cpu" sort by
// that single resource descending. Unknown values sort last, and the final
// tiebreak is name ascending, so output is deterministic.
func psSortRows(rows []psRow, sortBy string) {
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := psSortKey(rows[i], sortBy), psSortKey(rows[j], sortBy)
		if pi != pj {
			return pi > pj
		}
		if sortBy == "vram" {
			ri, rj := psRAMVal(rows[i]), psRAMVal(rows[j])
			if ri != rj {
				return ri > rj
			}
		}
		return rows[i].Name < rows[j].Name
	})
}

// psSortKey returns the primary sort value for a row and field. Unknown maps to
// -1 so it sorts after every real value (which are all >= 0) in descending order.
func psSortKey(r psRow, sortBy string) float64 {
	switch sortBy {
	case "ram":
		return psRAMVal(r)
	case "cpu":
		return psCPUVal(r)
	default: // "vram"
		return psVRAMVal(r)
	}
}

func psVRAMVal(r psRow) float64 {
	if r.VRAMBytes == nil {
		return -1
	}
	return float64(*r.VRAMBytes)
}

func psRAMVal(r psRow) float64 {
	if r.RAMBytes == nil {
		return -1
	}
	return float64(*r.RAMBytes)
}

func psCPUVal(r psRow) float64 {
	if r.CPUPercent == nil {
		return -1
	}
	return *r.CPUPercent
}

// formatPSTable renders rows as an aligned, plain-text table (no color codes, so
// the alignment is correct and the output is script-friendly; --no-color is
// respected globally and the table carries no color either way).
func formatPSTable(w io.Writer, rows []psRow) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tRUNTIME\tSTATUS\tCPU%\tRAM\tVRAM\tPORT\tPID\tNOTE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name,
			r.Kind,
			psDash(r.Runtime),
			r.Status,
			psFmtCPU(r.CPUPercent),
			psFmtBytes(r.RAMBytes),
			psFmtBytes(r.VRAMBytes),
			psFmtInt(r.Port),
			psFmtInt(r.PID),
			r.Note,
		)
	}
	tw.Flush()
}

// psDash renders an empty string as "-".
func psDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// psFmtCPU renders a CPU percentage, "-" when unknown.
func psFmtCPU(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// psFmtBytes renders a byte count as a compact "6.1G" (reusing the status
// package's formatter), "-" when unknown.
func psFmtBytes(b *uint64) string {
	if b == nil {
		return "-"
	}
	return status.FormatBytesGB(*b)
}

// psFmtInt renders a port/pid, "-" when zero (none / not exposed).
func psFmtInt(v int) string {
	if v == 0 {
		return "-"
	}
	return strconv.Itoa(v)
}
