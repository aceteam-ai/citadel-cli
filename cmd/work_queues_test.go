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
