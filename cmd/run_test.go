// cmd/run_test.go
package cmd

import (
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// TestServiceIsKnown verifies the validation gate used by `citadel run <name>`
// accepts both embedded/catalog services (services.ServiceMap) and
// module-installed services tracked in the manifest, while rejecting unknown
// names. This is the regression guard for issue #358, where the gate only
// consulted ServiceMap and rejected module-installed services like "tei".
func TestServiceIsKnown(t *testing.T) {
	// "tei" is a stand-in for a module-installed service: it is NOT in
	// services.ServiceMap but is present in the manifest after
	// `citadel module install tei`.
	manifest := &CitadelManifest{
		Services: []Service{
			{Name: "tei", ComposeFile: "./services/tei.yml"},
		},
	}

	// Sanity check: the module-installed name must not be an embedded service,
	// otherwise case (b) would not actually exercise the manifest fallback.
	if _, ok := services.ServiceMap["tei"]; ok {
		t.Fatal("test assumption broken: 'tei' should not be in services.ServiceMap")
	}

	tests := []struct {
		name        string
		serviceName string
		want        bool
	}{
		{"embedded service only (ServiceMap)", "vllm", true},
		{"module-installed service only (manifest)", "tei", true},
		{"unknown service", "definitely-not-a-service", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceIsKnown(tt.serviceName, manifest); got != tt.want {
				t.Errorf("serviceIsKnown(%q) = %v, want %v", tt.serviceName, got, tt.want)
			}
		})
	}
}

// TestKnownServiceNames verifies the "Available services" hint includes both
// embedded services and module-installed services from the manifest, deduped
// and in a stable (sorted) order.
func TestKnownServiceNames(t *testing.T) {
	manifest := &CitadelManifest{
		Services: []Service{
			{Name: "tei", ComposeFile: "./services/tei.yml"},
			// vllm is also embedded; it must not be duplicated.
			{Name: "vllm", ComposeFile: "./services/vllm.yml"},
		},
	}

	got := knownServiceNames(manifest)

	// Expect all embedded services (sorted) followed by the unique manifest-only
	// names (sorted), with no duplicates.
	want := append([]string{}, services.GetAvailableServices()...)
	want = append(want, "tei")

	if !reflect.DeepEqual(got, want) {
		t.Errorf("knownServiceNames() = %v, want %v", got, want)
	}

	// Explicit dedup check: "vllm" should appear exactly once.
	count := 0
	for _, n := range got {
		if n == "vllm" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("knownServiceNames() contains 'vllm' %d times, want 1", count)
	}
}
