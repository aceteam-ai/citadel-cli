package compose

import (
	"reflect"
	"sort"
	"testing"
)

// TestHostPorts_ShortAndLongForm verifies the parser extracts published host
// ports across the compose short and long forms, and skips bare/ephemeral
// publishes. This underpins the citadel-cli#415 assertion that a SERVICE_START
// container publishes the host port its compose declares.
func TestHostPorts_ShortAndLongForm(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []int
	}{
		{
			name: "short form host:container",
			input: `services:
  diffusers:
    image: x
    ports:
      - "8102:7860"`,
			want: []int{8102},
		},
		{
			name: "short form with protocol suffix",
			input: `services:
  svc:
    ports:
      - "5000:5000/tcp"`,
			want: []int{5000},
		},
		{
			name: "ip:host:container",
			input: `services:
  svc:
    ports:
      - "127.0.0.1:9000:9000"`,
			want: []int{9000},
		},
		{
			name: "long form published/target",
			input: `services:
  svc:
    ports:
      - target: 80
        published: 8080
        protocol: tcp`,
			want: []int{8080},
		},
		{
			name: "bare container port is ephemeral -> skipped",
			input: `services:
  svc:
    ports:
      - "7860"`,
			want: nil,
		},
		{
			name: "no ports section",
			input: `services:
  svc:
    image: x`,
			want: nil,
		},
		{
			name: "multiple services and ports deduped",
			input: `services:
  a:
    ports:
      - "8000:8000"
  b:
    ports:
      - "8000:8000"
      - "9000:9000"`,
			want: []int{8000, 9000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HostPorts([]byte(tt.input))
			sort.Ints(got)
			want := append([]int(nil), tt.want...)
			sort.Ints(want)
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("HostPorts() = %v, want %v", got, want)
			}
		})
	}
}

// TestHostPorts_InvalidYAML returns nil rather than panicking on malformed input.
func TestHostPorts_InvalidYAML(t *testing.T) {
	if got := HostPorts([]byte("::not yaml::")); got != nil {
		t.Errorf("HostPorts(invalid) = %v, want nil", got)
	}
}
