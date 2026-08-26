package servicediag

import "testing"

func TestIsManaged_Manifest(t *testing.T) {
	managed, source := IsManaged("my-custom-svc", []string{"my-custom-svc", "ollama"})
	if !managed || source != "manifest" {
		t.Errorf("IsManaged() = (%v, %q), want (true, manifest)", managed, source)
	}
}

func TestIsManaged_Catalog(t *testing.T) {
	// "vllm" is always in the embedded services.ServiceMap catalog.
	managed, source := IsManaged("vllm", nil)
	if !managed || source != "catalog" {
		t.Errorf("IsManaged() = (%v, %q), want (true, catalog)", managed, source)
	}
}

func TestIsManaged_Unmanaged(t *testing.T) {
	managed, source := IsManaged("some-random-adhoc-container", []string{"ollama"})
	if managed || source != "" {
		t.Errorf("IsManaged() = (%v, %q), want (false, \"\")", managed, source)
	}
}
