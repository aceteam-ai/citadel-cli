package cmd

import (
	"reflect"
	"testing"
)

func TestAppendUniqueQueues(t *testing.T) {
	base := []string{"jobs:v1:cpu-general", "jobs:v1:shell:org_x"}
	extra := []string{"jobs:v1:tag:gpu:rtx3090", "jobs:v1:gpu-general", "jobs:v1:cpu-general"}
	got := appendUniqueQueues(base, extra)
	want := []string{
		"jobs:v1:cpu-general",
		"jobs:v1:shell:org_x",
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:gpu-general",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("appendUniqueQueues() = %v, want %v", got, want)
	}
}

// TestMissingQueues pins the gate that decides whether the #612
// inference-queue reconciler gets built at all: a GPU node's boot-time queue
// set already contains its InferenceQueues(caps, true) result (because
// GPUInferenceQueues is unconditional on `serving`), so missingQueues must
// return empty for it -- building a reconciler anyway would probe
// nodeIsServingModels forever with nothing left to add.
func TestMissingQueues(t *testing.T) {
	tests := []struct {
		name     string
		desired  []string
		existing []string
		want     []string
	}{
		{
			name:     "fresh CPU-only node: nothing subscribed yet, gap is the whole desired set",
			desired:  []string{"jobs:v1:gpu-general"},
			existing: []string{"jobs:v1:cpu-general", "jobs:v1:shell:org_x"},
			want:     []string{"jobs:v1:gpu-general"},
		},
		{
			name:     "GPU node: boot path already subscribed the tag queues -- no gap",
			desired:  []string{"jobs:v1:tag:gpu:rtx3090", "jobs:v1:gpu-general"},
			existing: []string{"jobs:v1:cpu-general", "jobs:v1:tag:gpu:rtx3090", "jobs:v1:gpu-general"},
			want:     nil,
		},
		{
			name:     "already serving at boot: gpu-general already present -- no gap",
			desired:  []string{"jobs:v1:gpu-general"},
			existing: []string{"jobs:v1:cpu-general", "jobs:v1:gpu-general"},
			want:     nil,
		},
		{
			name:     "no desired queues at all: nothing to fill",
			desired:  nil,
			existing: []string{"jobs:v1:cpu-general"},
			want:     nil,
		},
		{
			name:     "de-duplicates the desired set",
			desired:  []string{"jobs:v1:gpu-general", "jobs:v1:gpu-general"},
			existing: nil,
			want:     []string{"jobs:v1:gpu-general"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := missingQueues(tt.desired, tt.existing)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("missingQueues(%v, %v) = %v, want %v", tt.desired, tt.existing, got, tt.want)
			}
		})
	}
}
