package status

import (
	"reflect"
	"testing"
)

func TestFilterVisibleGPUs(t *testing.T) {
	all := []GPUMetrics{{Index: 0, Name: "a"}, {Index: 1, Name: "b"}, {Index: 2, Name: "c"}}
	if got := filterVisibleGPUs(all, nil); !reflect.DeepEqual(got, all) {
		t.Fatalf("nil filter changed metrics: %v", got)
	}
	if got := filterVisibleGPUs(all, []string{"2", " 0"}); len(got) != 2 || got[0].Index != 0 || got[1].Index != 2 {
		t.Fatalf("filter = %v", got)
	}
	if got := filterVisibleGPUs(all, []string{}); len(got) != 0 {
		t.Fatalf("empty allocation reported %v", got)
	}
	if got := filterVisibleGPUs(all, []string{"GPU-aaaa"}); len(got) != 0 {
		t.Fatalf("unmappable entries must report none, got %v", got)
	}
}

const smiList = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaaa1111)
  MIG 3g.20gb     Device  0: (UUID: MIG-bbbb2222)
  MIG 3g.20gb     Device  1: (UUID: MIG-GPU-aaaa1111/2/0)
GPU 1: NVIDIA A100-SXM4-40GB (UUID: GPU-cccc3333)
GPU 2: NVIDIA A100-SXM4-40GB (UUID: GPU-dddd4444)
`

func TestParseGPUUUIDIndex(t *testing.T) {
	m := parseGPUUUIDIndex(smiList)
	want := map[string]int{"GPU-aaaa1111": 0, "MIG-bbbb2222": 0, "MIG-GPU-aaaa1111/2/0": 0, "GPU-cccc3333": 1, "GPU-dddd4444": 2}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("map = %v", m)
	}
}

func TestFilterVisibleGPUsUUIDAndMIG(t *testing.T) {
	all := []GPUMetrics{{Index: 0}, {Index: 1}, {Index: 2}}
	lookup := func() map[string]int { return parseGPUUUIDIndex(smiList) }
	idx := func(g []GPUMetrics) []int {
		var out []int
		for _, m := range g {
			out = append(out, m.Index)
		}
		return out
	}
	for name, tc := range map[string]struct {
		visible []string
		want    []int
	}{
		"gpu uuid":         {[]string{"GPU-cccc3333"}, []int{1}},
		"two uuids":        {[]string{"GPU-dddd4444", "GPU-aaaa1111"}, []int{0, 2}},
		"mig uuid":         {[]string{"MIG-bbbb2222"}, []int{0}},
		"mig with suffix":  {[]string{"MIG-GPU-aaaa1111/2/0"}, []int{0}},
		"mig parent form":  {[]string{"MIG-GPU-aaaa1111/9/9"}, []int{0}},
		"mixed index+uuid": {[]string{"2", "GPU-cccc3333"}, []int{1, 2}},
		"unknown uuid":     {[]string{"GPU-zzzz"}, nil},
	} {
		if got := idx(filterVisibleGPUsWithUUIDs(all, tc.visible, lookup)); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	// The listing is only consulted when a non-index entry exists.
	called := false
	filterVisibleGPUsWithUUIDs(all, []string{"0", "1"}, func() map[string]int { called = true; return nil })
	if called {
		t.Fatal("nvidia-smi would be run for a plain index list")
	}
}
