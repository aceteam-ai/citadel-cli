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
