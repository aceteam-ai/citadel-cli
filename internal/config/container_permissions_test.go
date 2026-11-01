package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainerHostPermissionIsSeparateAndDefaultDeny(t *testing.T) {
	dir := t.TempDir()
	p := LoadPermissions(dir)
	if p.ContainerHost || p.ContainerRuntime != "" || p.ContainerEgressProxyURL != "" {
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
	p.ContainerEgressProxyURL = "http://host.docker.internal:3128"
	if SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	loaded, err := LoadPermissionsStrict(dir)
	if err != nil || !loaded.ContainerHost || loaded.ContainerRuntime != "runsc" || loaded.ContainerEgressProxyURL != "http://host.docker.internal:3128" {
		t.Fatal("explicit owner grant did not round trip")
	}
}

func TestContainerEgressProxyUsesExactNodeConfigurationKey(t *testing.T) {
	dir := t.TempDir()
	body := "container_host: true\ncontainer_runtime: runsc\ncontainer_egress_proxy_url: http://host.docker.internal:3128\n"
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPermissionsStrict(dir)
	if err != nil || p.ContainerEgressProxyURL != "http://host.docker.internal:3128" || !p.ContainerHost || p.ContainerRuntime != "runsc" {
		t.Fatal("S5 node proxy configuration key was not honored")
	}
}
