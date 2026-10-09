// cmd/ps_cmd_test.go
//
// These tests exercise the pure row-building / sorting / formatting / JSON core
// of "citadel ps" over INJECTED *status.NodeStatus and *resmon.Snapshot
// fixtures. They never call runPs, findAndReadManifest, Collect, resmon.Collect,
// nvidia-smi, or any container runtime. This box runs a live citadel node and
// must not have a built binary or a live collection pointed at it.
package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/resmon"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// gib is defined once in the cmd test package (controlcenter_footprint_test.go)
// as uint64(1024*1024*1024); reused here.

func u64p(v uint64) *uint64 { return &v }

// fp is a terse ServiceFootprint constructor for fixtures.
func fp(cpu float64, ramBytes, vramBytes uint64, hasGPU bool) *status.ServiceFootprint {
	return &status.ServiceFootprint{
		CPUPercent:     cpu,
		RAMBytes:       ramBytes,
		VRAMBytes:      vramBytes,
		GPUUtilPercent: 80,
		HasGPU:         hasGPU,
	}
}

func rowByName(rows []psRow, name string) (psRow, bool) {
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return psRow{}, false
}

func TestBuildPSRows_ManagedServicesAndApps(t *testing.T) {
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "vllm", Status: status.ServiceStatusRunning, Port: 8000, Footprint: fp(74, 6*gib, 21*gib, true)},
			// Native / adopted-external engine: running, no container stats.
			{Name: "ollama", Status: status.ServiceStatusRunning, Port: 11434, Footprint: nil},
			// Stopped service must be excluded (ps lists running workloads only).
			{Name: "diffusers", Status: status.ServiceStatusStopped, Footprint: fp(0, 2*gib, 3*gib, true)},
		},
		Apps: []status.AppInfo{
			{Name: "openwebui", Status: status.ServiceStatusRunning, Port: 8080, Footprint: fp(1, 512*1024*1024, 0, true)},
		},
	}

	rows := buildPSRows(st, nil, "docker", psOptions{system: false, sortBy: "vram"})

	if len(rows) != 3 {
		t.Fatalf("want 3 running rows (stopped excluded), got %d: %+v", len(rows), rows)
	}

	// Default VRAM-desc order: vllm (21G) > openwebui (0, GPU) > ollama (unknown).
	if rows[0].Name != "vllm" || rows[1].Name != "openwebui" || rows[2].Name != "ollama" {
		t.Fatalf("unexpected VRAM-desc order: %s, %s, %s", rows[0].Name, rows[1].Name, rows[2].Name)
	}

	vllm, _ := rowByName(rows, "vllm")
	if vllm.Kind != string(status.EntityService) {
		t.Errorf("vllm kind = %q, want service", vllm.Kind)
	}
	if vllm.Runtime != "vllm" {
		t.Errorf("vllm runtime = %q, want vllm (engine dialect from name)", vllm.Runtime)
	}
	if vllm.VRAMBytes == nil || *vllm.VRAMBytes != 21*gib {
		t.Errorf("vllm VRAM = %v, want %d", vllm.VRAMBytes, 21*gib)
	}
	if vllm.RAMBytes == nil || vllm.CPUPercent == nil {
		t.Errorf("vllm RAM/CPU should be populated from footprint: ram=%v cpu=%v", vllm.RAMBytes, vllm.CPUPercent)
	}
	if vllm.Port != 8000 {
		t.Errorf("vllm port = %d, want 8000", vllm.Port)
	}

	// ollama: engine dialect resolves from the name even with no footprint, but
	// CPU/RAM/VRAM stay unknown (nil) rather than a fabricated 0.
	ollama, _ := rowByName(rows, "ollama")
	if ollama.Runtime != "ollama" {
		t.Errorf("ollama runtime = %q, want ollama", ollama.Runtime)
	}
	if ollama.VRAMBytes != nil || ollama.RAMBytes != nil || ollama.CPUPercent != nil {
		t.Errorf("ollama with nil footprint must have unknown CPU/RAM/VRAM, got vram=%v ram=%v cpu=%v",
			ollama.VRAMBytes, ollama.RAMBytes, ollama.CPUPercent)
	}

	// openwebui app: runtime is the container runtime (footprint present); VRAM is
	// a real 0 on a GPU node (not unknown).
	app, _ := rowByName(rows, "openwebui")
	if app.Kind != string(status.EntityApp) {
		t.Errorf("openwebui kind = %q, want app", app.Kind)
	}
	if app.Runtime != "docker" {
		t.Errorf("openwebui runtime = %q, want docker", app.Runtime)
	}
	if app.VRAMBytes == nil || *app.VRAMBytes != 0 {
		t.Errorf("openwebui VRAM = %v, want a real 0 (GPU node)", app.VRAMBytes)
	}
}

func TestBuildPSRows_NoGPURendersVRAMDash(t *testing.T) {
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "tei", Status: status.ServiceStatusRunning, Port: 8081, Footprint: fp(3, 1*gib, 0, false)},
		},
	}
	rows := buildPSRows(st, nil, "docker", psOptions{sortBy: "vram"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].VRAMBytes != nil {
		t.Errorf("no-GPU footprint must leave VRAM unknown (nil), got %v", rows[0].VRAMBytes)
	}

	var buf bytes.Buffer
	formatPSTable(&buf, rows)
	out := buf.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "VRAM") {
		t.Fatalf("table missing header:\n%s", out)
	}
	// The VRAM cell for the tei row must render as "-" (not "0.0G").
	var teiLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "tei") {
			teiLine = line
		}
	}
	if teiLine == "" {
		t.Fatalf("no tei row in table:\n%s", out)
	}
	if strings.Contains(teiLine, "0.0G") {
		t.Errorf("no-GPU VRAM must render as '-', not 0.0G: %q", teiLine)
	}
	if !strings.Contains(teiLine, "-") {
		t.Errorf("expected a '-' cell in no-GPU row: %q", teiLine)
	}
}

func TestBuildPSRows_SystemIncludesNonManagedExcludesManaged(t *testing.T) {
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "vllm", Status: status.ServiceStatusRunning, Port: 8000, Footprint: fp(50, 6*gib, 21*gib, true)},
		},
	}
	snap := &resmon.Snapshot{
		HasGPU: true,
		Consumers: []resmon.Consumer{
			// A model started outside citadel (the incident's qwen holder).
			{PID: 4242, Owner: "host:ollama", Kind: resmon.OwnerHost, VRAMBytes: 20 * gib, RSSBytes: 2 * gib, CPUPercent: -1, Reclaimable: true, Reason: "idle 40m holding 20.0G VRAM"},
			{PID: 5555, Owner: "container:open-webui", Kind: resmon.OwnerContainer, VRAMBytes: 1 * gib, RSSBytes: 500 * 1024 * 1024, CPUPercent: -1},
			// Managed consumer: must NOT be duplicated as a system row (already a
			// managed service row above).
			{PID: 6666, Owner: "citadel-managed", Kind: resmon.OwnerCitadelManaged, VRAMBytes: 21 * gib, RSSBytes: 6 * gib, CPUPercent: -1},
		},
	}

	// system=false: no system rows even though a snapshot was passed.
	noSys := buildPSRows(st, snap, "docker", psOptions{system: false, sortBy: "vram"})
	for _, r := range noSys {
		if r.Kind == "system" {
			t.Fatalf("system rows must not appear without --system: %+v", r)
		}
	}
	if len(noSys) != 1 {
		t.Fatalf("want 1 managed row without --system, got %d", len(noSys))
	}

	// system=true: vllm (managed) + 2 non-managed system rows; managed consumer
	// excluded.
	rows := buildPSRows(st, snap, "docker", psOptions{system: true, sortBy: "vram"})
	var sysRows []psRow
	for _, r := range rows {
		if r.Kind == "system" {
			sysRows = append(sysRows, r)
		}
	}
	if len(sysRows) != 2 {
		t.Fatalf("want 2 system rows (managed excluded), got %d: %+v", len(sysRows), sysRows)
	}
	if _, ok := rowByName(rows, "citadel-managed"); ok {
		t.Errorf("managed consumer must not appear as a system row")
	}

	host, ok := rowByName(rows, "host:ollama")
	if !ok {
		t.Fatalf("expected host:ollama system row")
	}
	if host.Runtime != "host" {
		t.Errorf("host:ollama runtime = %q, want host", host.Runtime)
	}
	if host.PID != 4242 {
		t.Errorf("host:ollama pid = %d, want 4242", host.PID)
	}
	if !host.Reclaimable || host.Note == "" {
		t.Errorf("host:ollama should be reclaimable with a reason: %+v", host)
	}
	if host.VRAMBytes == nil || *host.VRAMBytes != 20*gib {
		t.Errorf("host:ollama VRAM = %v, want %d", host.VRAMBytes, 20*gib)
	}
	// resmon does not sample per-process CPU in v1, so CPU is unknown.
	if host.CPUPercent != nil {
		t.Errorf("system row CPU should be unknown (resmon v1), got %v", host.CPUPercent)
	}

	// Default VRAM-desc order across managed + system: vllm(21) > host(20) > container(1).
	if rows[0].Name != "vllm" || rows[1].Name != "host:ollama" || rows[2].Name != "container:open-webui" {
		t.Fatalf("unexpected combined VRAM-desc order: %s, %s, %s", rows[0].Name, rows[1].Name, rows[2].Name)
	}
}

func TestPSSortRows(t *testing.T) {
	mk := func(name string, cpu *float64, ram, vram *uint64) psRow {
		return psRow{Name: name, CPUPercent: cpu, RAMBytes: ram, VRAMBytes: vram}
	}
	cpu := func(v float64) *float64 { return &v }

	t.Run("ram desc, unknown last, name tiebreak", func(t *testing.T) {
		rows := []psRow{
			mk("b", nil, u64p(5*gib), nil),
			mk("c", nil, nil, nil), // unknown RAM -> last
			mk("a", nil, u64p(5*gib), nil),
			mk("d", nil, u64p(10*gib), nil),
		}
		psSortRows(rows, "ram")
		got := []string{rows[0].Name, rows[1].Name, rows[2].Name, rows[3].Name}
		want := []string{"d", "a", "b", "c"} // 10G; then 5G tie -> a,b by name; unknown last
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("ram sort = %v, want %v", got, want)
		}
	})

	t.Run("cpu desc", func(t *testing.T) {
		rows := []psRow{
			mk("low", cpu(5), nil, nil),
			mk("high", cpu(90), nil, nil),
			mk("none", nil, nil, nil),
		}
		psSortRows(rows, "cpu")
		if rows[0].Name != "high" || rows[1].Name != "low" || rows[2].Name != "none" {
			t.Errorf("cpu sort = %s,%s,%s", rows[0].Name, rows[1].Name, rows[2].Name)
		}
	})

	t.Run("vram desc then ram desc", func(t *testing.T) {
		rows := []psRow{
			mk("x", nil, u64p(1*gib), u64p(5*gib)),
			mk("y", nil, u64p(9*gib), u64p(5*gib)), // same VRAM, higher RAM -> first
			mk("z", nil, u64p(50*gib), u64p(10*gib)),
		}
		psSortRows(rows, "vram")
		if rows[0].Name != "z" || rows[1].Name != "y" || rows[2].Name != "x" {
			t.Errorf("vram sort = %s,%s,%s", rows[0].Name, rows[1].Name, rows[2].Name)
		}
	})
}

func TestPSJSON_ShapeAndOmitEmpty(t *testing.T) {
	// Empty node: workloads must serialize as [] (not null) so scripts can index it.
	emptyRows := buildPSRows(&status.NodeStatus{}, nil, "docker", psOptions{sortBy: "vram"})
	emptyBytes, err := json.Marshal(psJSONOutput{Node: "node-1297", Workloads: emptyRows})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if !strings.Contains(string(emptyBytes), `"workloads":[]`) {
		t.Errorf("empty workloads must be [] not null: %s", emptyBytes)
	}

	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "vllm", Status: status.ServiceStatusRunning, Port: 8000, Footprint: fp(74, 6*gib, 21*gib, true)},
			{Name: "ollama", Status: status.ServiceStatusRunning, Port: 11434, Footprint: nil},
		},
	}
	rows := buildPSRows(st, nil, "docker", psOptions{sortBy: "vram"})
	data, err := json.Marshal(psJSONOutput{Node: "node-1297", Workloads: rows})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out struct {
		Node      string `json:"node"`
		Workloads []struct {
			Name       string   `json:"name"`
			Kind       string   `json:"kind"`
			Runtime    string   `json:"runtime"`
			Status     string   `json:"status"`
			CPUPercent *float64 `json:"cpu_percent"`
			RAMBytes   *uint64  `json:"ram_bytes"`
			VRAMBytes  *uint64  `json:"vram_bytes"`
			Port       int      `json:"port"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Node != "node-1297" || len(out.Workloads) != 2 {
		t.Fatalf("unexpected json: node=%q workloads=%d", out.Node, len(out.Workloads))
	}

	// vllm is first (VRAM desc); its raw byte counts are present as numbers.
	v := out.Workloads[0]
	if v.Name != "vllm" || v.VRAMBytes == nil || *v.VRAMBytes != 21*gib {
		t.Errorf("vllm json VRAM = %v, want %d", v.VRAMBytes, 21*gib)
	}
	if v.RAMBytes == nil || v.CPUPercent == nil {
		t.Errorf("vllm json RAM/CPU should be present")
	}

	// ollama's unknown CPU/RAM/VRAM must be ABSENT (omitempty on nil pointers).
	// Find it by scanning the raw object keys.
	var raw struct {
		Workloads []map[string]json.RawMessage `json:"workloads"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, w := range raw.Workloads {
		name := ""
		_ = json.Unmarshal(w["name"], &name)
		if name != "ollama" {
			continue
		}
		for _, k := range []string{"cpu_percent", "ram_bytes", "vram_bytes"} {
			if _, present := w[k]; present {
				t.Errorf("ollama json should omit unknown %q, but it is present", k)
			}
		}
		if _, present := w["runtime"]; !present {
			t.Errorf("ollama json should carry runtime (ollama)")
		}
	}
}

// TestFormatPSTable_Sample renders a realistic table and logs it. The logged
// output doubles as the PR-body sample, produced purely from fixtures with no
// live node, GPU, or container runtime.
func TestFormatPSTable_Sample(t *testing.T) {
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "vllm", Status: status.ServiceStatusRunning, Port: 8000, Footprint: fp(74, 6*gib, 21*gib, true)},
			{Name: "ollama", Status: status.ServiceStatusRunning, Port: 11434, Footprint: nil},
		},
		Apps: []status.AppInfo{
			{Name: "openwebui", Status: status.ServiceStatusRunning, Port: 8080, Footprint: fp(1, 512*1024*1024, 0, true)},
		},
	}
	snap := &resmon.Snapshot{
		HasGPU: true,
		Consumers: []resmon.Consumer{
			{PID: 4242, Owner: "host:ollama", Kind: resmon.OwnerHost, VRAMBytes: 20 * gib, RSSBytes: 2 * gib, CPUPercent: -1, Reclaimable: true, Reason: "idle 40m holding 20.0G VRAM"},
		},
	}
	rows := buildPSRows(st, snap, "docker", psOptions{system: true, sortBy: "vram"})
	var buf bytes.Buffer
	formatPSTable(&buf, rows)
	out := buf.String()
	if !strings.HasPrefix(out, "NAME") {
		t.Fatalf("table should start with header NAME, got:\n%s", out)
	}
	t.Logf("citadel ps --system sample:\n%s", out)
}
