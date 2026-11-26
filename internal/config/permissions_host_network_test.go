package config

import "testing"

func TestHostNetworkPermissionDefaultAndPersistence(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadPermissionsStrict(dir)
	if err != nil || p.HostNetwork {
		t.Fatalf("default = %+v, %v", p, err)
	}
	p.HostNetwork = true
	if err := SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPermissionsStrict(dir)
	if err != nil || !loaded.HostNetwork {
		t.Fatalf("saved = %+v, %v", loaded, err)
	}
}
