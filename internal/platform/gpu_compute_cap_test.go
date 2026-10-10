package platform

import (
	"reflect"
	"testing"
)

// TestParseGPUComputeCaps drives the pure parser against captured
// nvidia-smi --query-gpu=compute_cap --format=csv,noheader output so the
// TEI image-tag resolution never shells out to a real GPU in a test.
func TestParseGPUComputeCaps(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "8.6\n", []string{"8.6"}},
		{"single-no-newline", "8.6", []string{"8.6"}},
		{"homogeneous-multi", "8.6\n8.6\n", []string{"8.6", "8.6"}},
		{"heterogeneous", "7.0\n8.6\n", []string{"7.0", "8.6"}},
		{"blank-lines-skipped", "\n8.9\n\n", []string{"8.9"}},
		{"whitespace-trimmed", " 9.0 \r\n", []string{"9.0"}},
		{"empty", "", nil},
		{"only-blanks", "\n\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseGPUComputeCaps(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseGPUComputeCaps(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
