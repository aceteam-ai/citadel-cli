package config

import "testing"

func TestContainerHostPermissionIsSeparateAndDefaultDeny(t *testing.T) {
	dir := t.TempDir()
	p := LoadPermissions(dir)
	if p.ContainerHost || p.ContainerRuntime != "" {
		t.Fatal("default host permission broadened")
	}
	p.Shell = true
	p.Files = true
	if SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	if LoadPermissions(dir).ContainerHost {
		t.Fatal("host permission inherited from shell/files")
	}
	p.ContainerHost = true
	p.ContainerRuntime = "runsc"
	if SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	loaded, err := LoadPermissionsStrict(dir)
	if err != nil || !loaded.ContainerHost || loaded.ContainerRuntime != "runsc" {
		t.Fatal("explicit owner grant did not round trip")
	}
}
