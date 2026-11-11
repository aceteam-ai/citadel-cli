package hpc

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func slurmEnv(over map[string]string) func(string) string {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	env := map[string]string{
		EnvClusterName: "testcluster",
		EnvPartition:   "gpu",
		EnvAccount:     "lab-a",
		EnvJobID:       "4242",
		EnvNodeName:    "gpu-node-07",
		EnvVisibleGPUs: "0,1",
		EnvJobEndTime:  strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
	}
	for k, v := range over {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

var labelNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func TestResolveLabelsFromSlurmEnv(t *testing.T) {
	var gotVisible []string
	l, err := ResolveLabels(LabelInputs{
		Getenv: slurmEnv(nil),
		GPUNames: func(v []string) []string {
			gotVisible = v
			return []string{"NVIDIA A100-SXM4-80GB", "NVIDIA A100-SXM4-80GB"}
		},
		Now: labelNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotVisible, []string{"0", "1"}) {
		t.Fatalf("probe saw %v", gotVisible)
	}
	m := l.Map()
	want := map[string]any{
		"cluster": "testcluster", "partition": "gpu", "account": "lab-a", "slurm_job_id": "4242",
		"host": "gpu-node-07", "gpu_type": "NVIDIA-A100-SXM4-80GB", "allocated_gpus": 2,
		"wall_time_end": "2026-11-12T12:00:00Z",
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("labels = %#v\nwant %#v", m, want)
	}
}

// TestLabelKeysMatchPlatformSchema pins the exact key set the platform's
// strict schema accepts; any extra key is a 400 after the token burns.
func TestLabelKeysMatchPlatformSchema(t *testing.T) {
	l, err := ResolveLabels(LabelInputs{Getenv: slurmEnv(map[string]string{EnvVisibleGPUs: ""}), Now: labelNow})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range l.Map() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"account", "allocated_gpus", "cluster", "host", "partition", "slurm_job_id", "wall_time_end"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v (gpu_type omitted on a CPU job)", keys, want)
	}
	if l.AllocatedGPUs != 0 {
		t.Fatalf("allocated_gpus = %d", l.AllocatedGPUs)
	}
}

func TestResolveLabelsRefusals(t *testing.T) {
	cases := map[string]struct {
		over     map[string]string
		wall     string
		wantPart string
	}{
		"no cluster":     {over: map[string]string{EnvClusterName: ""}, wantPart: EnvClusterName + " is not set"},
		"no account":     {over: map[string]string{EnvAccount: ""}, wantPart: EnvAccount},
		"bad partition":  {over: map[string]string{EnvPartition: "gpu long"}, wantPart: "characters"},
		"no end time":    {over: map[string]string{EnvJobEndTime: ""}, wantPart: "no time limit"},
		"bad end time":   {over: map[string]string{EnvJobEndTime: "soon"}, wantPart: "Unix time"},
		"ending now":     {over: map[string]string{EnvJobEndTime: strconv.FormatInt(labelNow.Add(30*time.Second).Unix(), 10)}, wantPart: "less than a minute"},
		"too far":        {over: map[string]string{EnvJobEndTime: strconv.FormatInt(labelNow.Add(8*24*time.Hour).Unix(), 10)}, wantPart: "7 days"},
		"bad override":   {over: map[string]string{EnvJobEndTime: ""}, wall: "tomorrow", wantPart: "RFC 3339"},
		"no host at all": {over: map[string]string{EnvNodeName: ""}, wantPart: "hostname"},
	}
	for name, tc := range cases {
		_, err := ResolveLabels(LabelInputs{Getenv: slurmEnv(tc.over), WallTimeEnd: tc.wall, Now: labelNow})
		if err == nil || !strings.Contains(err.Error(), tc.wantPart) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.wantPart)
		}
	}
}

func TestResolveLabelsOverridesAndFallbacks(t *testing.T) {
	l, err := ResolveLabels(LabelInputs{
		Getenv:      slurmEnv(map[string]string{EnvJobEndTime: "", EnvNodeName: ""}),
		Hostname:    "gpu node.example",
		WallTimeEnd: "2026-11-12T11:30:00Z",
		Now:         labelNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.Host != "gpu-node.example" || !l.WallTimeEnd.Equal(time.Date(2026, 11, 12, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("got host %q end %v", l.Host, l.WallTimeEnd)
	}
	if l.GPUType != "" {
		t.Fatalf("no probe should leave gpu_type empty, got %q", l.GPUType)
	}
}

func TestVisibleGPUsAndNames(t *testing.T) {
	if got := VisibleGPUs(" 2, 3 ,"); !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Fatalf("VisibleGPUs = %v", got)
	}
	if got := VisibleGPUs("NoDevFiles"); len(got) != 0 {
		t.Fatalf("NoDevFiles = %v", got)
	}
	csv := "0, GPU-aaaa, NVIDIA A100-SXM4-80GB\n1, GPU-bbbb, NVIDIA H100 80GB HBM3\n"
	if got := ParseGPUNames(csv, []string{"1"}); !reflect.DeepEqual(got, []string{"NVIDIA H100 80GB HBM3"}) {
		t.Fatalf("by index = %v", got)
	}
	if got := ParseGPUNames(csv, []string{"GPU-aaaa"}); !reflect.DeepEqual(got, []string{"NVIDIA A100-SXM4-80GB"}) {
		t.Fatalf("by uuid = %v", got)
	}
	if SanitizeLabel("  ***  ") != "" || SanitizeLabel("Tesla V100 (32GB)") != "Tesla-V100-32GB" {
		t.Fatalf("sanitize: %q %q", SanitizeLabel("  ***  "), SanitizeLabel("Tesla V100 (32GB)"))
	}
	if long := SanitizeLabel(strings.Repeat("a", 300)); len(long) != 128 {
		t.Fatalf("sanitize length %d", len(long))
	}
}
