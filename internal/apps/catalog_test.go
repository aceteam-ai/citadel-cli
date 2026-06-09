package apps

import "testing"

func TestLookup(t *testing.T) {
	tests := []struct {
		name  string
		found bool
	}{
		{"code-server", true},
		{"jupyter", true},
		{"filebrowser", true},
		{"ollama-webui", true},
		{"nonexistent", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := Lookup(tt.name)
			if ok != tt.found {
				t.Errorf("Lookup(%q) found = %v, want %v", tt.name, ok, tt.found)
			}
			if ok && m.Name != tt.name {
				t.Errorf("Lookup(%q).Name = %q, want %q", tt.name, m.Name, tt.name)
			}
		})
	}
}

func TestLookupManifestFields(t *testing.T) {
	m, ok := Lookup("code-server")
	if !ok {
		t.Fatal("code-server not found in catalog")
	}

	if m.Image == "" {
		t.Error("code-server image is empty")
	}
	if m.Description == "" {
		t.Error("code-server description is empty")
	}
	if len(m.Ports) == 0 {
		t.Error("code-server has no port mappings")
	}
	if len(m.Volumes) == 0 {
		t.Error("code-server has no volume mounts")
	}
	if m.HealthCheck == nil {
		t.Error("code-server has no health check")
	}
}

func TestList(t *testing.T) {
	names := List()
	if len(names) != 4 {
		t.Errorf("List() returned %d apps, want 4", len(names))
	}

	// Verify sorted order.
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("List() not sorted: %q comes after %q", names[i], names[i-1])
		}
	}
}

func TestAll(t *testing.T) {
	all := All()
	if len(all) != 4 {
		t.Errorf("All() returned %d apps, want 4", len(all))
	}

	// Verify that mutating the returned map doesn't affect the catalog.
	delete(all, "code-server")
	if _, ok := Lookup("code-server"); !ok {
		t.Error("deleting from All() result affected the builtin catalog")
	}
}

func TestAllAppsHaveRequiredFields(t *testing.T) {
	for _, name := range List() {
		m, _ := Lookup(name)
		if m.Image == "" {
			t.Errorf("app %q has empty Image", name)
		}
		if m.Description == "" {
			t.Errorf("app %q has empty Description", name)
		}
		if len(m.Ports) == 0 {
			t.Errorf("app %q has no port mappings", name)
		}
	}
}
