package status

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCapacityHeartbeatWireFields(t *testing.T) {
	// Exercise the actual heartbeat model, not a parallel projection. Known
	// zeros must survive while a missing source stays absent.
	const input = `{"load1":0,"load5":2.5,"load15":1.25,"ncpu":8,"psi_cpu_some_avg60":0,"psi_io_some_avg60":3.5,"psi_io_full_avg60":1.5,"psi_mem_some_avg60":4.5}`
	var metrics SystemMetrics
	if err := json.Unmarshal([]byte(input), &metrics); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(NodeStatus{System: metrics})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		System map[string]float64 `json:"system"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	var expected map[string]float64
	if err := json.Unmarshal([]byte(input), &expected); err != nil {
		t.Fatal(err)
	}
	for key, value := range expected {
		got, present := wire.System[key]
		if !present || got != value {
			t.Errorf("missing or changed optional heartbeat field %s: got=%v present=%v", key, got, present)
		}
	}
	// Decode into a fresh value: encoding/json intentionally preserves fields
	// absent from an update to an already-populated struct.
	metrics = SystemMetrics{}
	raw, err = json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	var empty map[string]float64
	if err := json.Unmarshal(raw, &empty); err != nil {
		t.Fatal(err)
	}
	for key := range expected {
		if _, present := empty[key]; present {
			t.Errorf("unknown %s fabricated a value", key)
		}
	}
}

func TestCapacityLoadParser(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"35.25 12.50 0.00 2/440 123\n", true}, {"0 0 0 1/1 1\n", true},
		{"1 2", false}, {"1 2 3 4/5 6 extra", false}, {"", false},
		{"NaN 2 3 1/2 3", false}, {"1 +Inf 3 1/2 3", false}, {"1 2 -Inf 1/2 3", false},
		{"-1 2 3 1/2 3", false}, {"1 -2 3 1/2 3", false}, {"1 2 -3 1/2 3", false},
		{"name 2 3 1/2 3", false}, {strings.Repeat(" ", capacityReadLimit) + "0 0 0 1/1 1", false},
	} {
		got, err := parseCapacityLoad([]byte(tc.raw))
		if (err == nil) != tc.valid {
			t.Fatalf("load validity changed for fixture: %v", err)
		}
		if tc.valid && strings.HasPrefix(tc.raw, "35.") && got != [3]float64{35.25, 12.5, 0} {
			t.Fatal("load intervals or units swapped")
		}
	}
}

func TestCapacityPressureParser(t *testing.T) {
	for _, tc := range []struct {
		name, raw  string
		some, full *float64
	}{
		{"intervals", "some avg10=99 avg60=3.5 avg300=88 total=123\nfull avg10=4 avg60=1.5 avg300=5 total=99\n", floatPtr(3.5), floatPtr(1.5)},
		{"zero", "some avg60=0\nfull avg60=100\n", floatPtr(0), floatPtr(100)},
		{"no-full", "some avg60=4.5\n", floatPtr(4.5), nil},
		{"unsupported", "", nil, nil}, {"missing", "some avg10=5 avg300=3 total=2", nil, nil},
		{"unknown-kind", "fixture-process avg60=12", nil, nil},
		{"nan", "some avg60=NaN\nfull avg60=2", nil, floatPtr(2)},
		{"infinite", "some avg60=+Inf", nil, nil}, {"negative", "some avg60=-1", nil, nil},
		{"percent", "some avg60=101", nil, nil}, {"invalid", "some avg60=invalid", nil, nil},
		{"duplicate-field", "some avg60=1 avg60=2", nil, nil},
		{"duplicate-row", "some avg60=1\nsome avg60=2\nsome avg60=3\nfull avg60=4", nil, floatPtr(4)},
		{"oversize", "some avg60=1\n" + strings.Repeat(" ", capacityReadLimit), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			some, full := parseCapacityPressure([]byte(tc.raw))
			if !reflect.DeepEqual(some, tc.some) || !reflect.DeepEqual(full, tc.full) {
				t.Fatal("pressure interval, validity or absence changed")
			}
		})
	}
}

func floatPtr(v float64) *float64 { return &v }

func TestCapacityProcCollection(t *testing.T) {
	fixtures := map[string]string{
		"/proc/loadavg":         "35 12.5 8 1/99 123\n",
		"/proc/pressure/cpu":    "some avg10=90 avg60=1 avg300=80 total=999\nfull avg60=44\n",
		"/proc/pressure/io":     "some avg60=2\nfull avg60=3\n",
		"/proc/pressure/memory": "some avg60=4\nfull avg60=55\n",
	}
	var paths []string
	probes := procCapacityProbes(func(path string) ([]byte, error) {
		paths = append(paths, path)
		value, ok := fixtures[path]
		if !ok {
			t.Fatal("unexpected probe path")
		}
		return []byte(value), nil
	}, func() int { return 8 })
	m := collectCapacityMetrics(probes)
	if m.NCPU == nil || *m.NCPU != 8 || m.Load1 == nil || *m.Load1 != 35 || m.Load5 == nil || *m.Load5 != 12.5 || m.Load15 == nil || *m.Load15 != 8 {
		t.Fatal("load or logical CPU count changed")
	}
	if m.PSICPUSomeAvg60 == nil || *m.PSICPUSomeAvg60 != 1 || m.PSIIOSomeAvg60 == nil || *m.PSIIOSomeAvg60 != 2 || m.PSIIOFullAvg60 == nil || *m.PSIIOFullAvg60 != 3 || m.PSIMemorySomeAvg60 == nil || *m.PSIMemorySomeAvg60 != 4 {
		t.Fatal("resource pressure mapped to wrong wire field")
	}
	if !reflect.DeepEqual(paths, []string{"/proc/loadavg", "/proc/pressure/cpu", "/proc/pressure/io", "/proc/pressure/memory"}) {
		t.Fatal("probe source changed")
	}
	if _, err := probes.pressure("../fixture-hostname"); err == nil || len(paths) != 4 {
		t.Fatal("unknown source opened a caller-selected path")
	}
	fixtures["/proc/loadavg"] = "malformed"
	fixtures["/proc/pressure/cpu"] = "some avg60=NaN"
	fixtures["/proc/pressure/io"] = "some avg60=2\nfull avg60=-1"
	m = collectCapacityMetrics(probes)
	if m.Load1 != nil || m.PSICPUSomeAvg60 != nil || m.PSIIOFullAvg60 != nil || m.PSIIOSomeAvg60 == nil || m.PSIMemorySomeAvg60 == nil || m.NCPU == nil {
		t.Fatal("bad optional source poisoned other observations or fabricated zero")
	}
}

func TestCapacityMissingSources(t *testing.T) {
	probes := procCapacityProbes(func(string) ([]byte, error) { return nil, os.ErrNotExist }, func() int { return 8 })
	m := collectCapacityMetrics(probes)
	if m.Load1 != nil || m.Load5 != nil || m.Load15 != nil || m.PSICPUSomeAvg60 != nil || m.PSIIOSomeAvg60 != nil || m.PSIIOFullAvg60 != nil || m.PSIMemorySomeAvg60 != nil || m.NCPU == nil {
		t.Fatal("missing procfs/PSI fabricated a healthy value")
	}
	for _, n := range []int{0, -1} {
		m = collectCapacityMetrics(capacityProbes{ncpu: func() int { return n }})
		if m.NCPU != nil {
			t.Fatal("unknown core count published")
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), -1} {
		m = collectCapacityMetrics(capacityProbes{load: func() ([3]float64, error) { return [3]float64{0, value, 0}, nil }})
		if m.Load1 != nil || m.Load5 != nil || m.Load15 != nil {
			t.Fatal("invalid optional platform load escaped")
		}
		if _, err := json.Marshal(NodeStatus{System: m}); err != nil {
			t.Fatal("bad optional telemetry rejected heartbeat")
		}
	}
	m = collectCapacityMetrics(capacityProbes{load: func() ([3]float64, error) { return [3]float64{1, 2, 3}, errors.New("fixture private hostname") }, pressure: func(string) ([]byte, error) { return []byte("some avg60=7"), errors.New("fixture private process") }})
	raw, err := json.Marshal(NodeStatus{System: m})
	if err != nil || m.Load1 != nil || m.PSICPUSomeAvg60 != nil || strings.Contains(string(raw), "fixture private") {
		t.Fatal("failed probe published data or diagnostics")
	}
}

type countingCapacityReader struct{ n int }

func (r *countingCapacityReader) Read(p []byte) (int, error) {
	left := capacityReadLimit + 2 - r.n
	if left <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), left)
	for i := range p[:n] {
		p[i] = ' '
	}
	r.n += n
	return n, nil
}

func TestCapacityReadBounds(t *testing.T) {
	if capacityReadLimit != 4096 {
		t.Fatal("source read budget changed")
	}
	reader := &countingCapacityReader{}
	if _, err := readCapacityInput(reader); err == nil || reader.n != capacityReadLimit+1 {
		t.Fatal("source allocation/read was not bounded before parsing")
	}
	if raw, err := readCapacityInput(strings.NewReader(strings.Repeat(" ", capacityReadLimit))); err != nil || len(raw) != capacityReadLimit {
		t.Fatal("exact-bound input refused")
	}
	path := filepath.Join(t.TempDir(), "kernel-fixture")
	if err := os.WriteFile(path, []byte("0 0 0 1/1 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readCapacityFile(path); err != nil || string(raw) != "0 0 0 1/1 1\n" {
		t.Fatal("fixture read failed")
	}
	if _, err := readCapacityFile(path + "-missing"); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestCapacityCollectorWiring(t *testing.T) {
	// Source audit avoids calling Collect(), which would sample the real host.
	f, err := parser.ParseFile(token.NewFileSet(), "collector.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "collectSystemMetrics" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			a, ok := n.(*ast.AssignStmt)
			if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
				return true
			}
			id, ok := a.Lhs[0].(*ast.Ident)
			if !ok || id.Name != "metrics" {
				return true
			}
			call, ok := a.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if ok && callee.Name == "collectCapacityMetrics" && len(call.Args) == 1 {
				native, ok := call.Args[0].(*ast.CallExpr)
				if ok {
					name, ok := native.Fun.(*ast.Ident)
					found = ok && name.Name == "nativeCapacityProbes"
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("heartbeat collector does not initialize capacity from the native source")
	}
}
