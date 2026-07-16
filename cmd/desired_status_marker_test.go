package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// readServicesFromManifest re-reads the manifest written by the helper and
// returns the services list, so tests can assert on the durable marker.
func readServicesFromManifest(t *testing.T, nodeDir string) []Service {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(nodeDir, "citadel.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m CitadelManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m.Services
}

func findServiceByName(services []Service, name string) *Service {
	for i := range services {
		if services[i].Name == name {
			return &services[i]
		}
	}
	return nil
}

// TestCCStopServiceSetsDurableMarker is the regression test for #528's
// persistence half: the TUI stop must record desired_status: stopped in
// citadel.yaml BEFORE tearing containers down, so a `citadel work` restart or
// reboot does not resurrect the service. The compose call itself may fail in
// this environment (no docker / no compose file) -- the marker must be set
// regardless. Uses a synthetic service name so no real container is touched.
func TestCCStopServiceSetsDurableMarker(t *testing.T) {
	nodeDir := writeManifestWithServices(t, []Service{
		{Name: "stub-528", Type: "docker", ComposeFile: filepath.Join("services", "stub-528.yml")},
	})

	_ = ccStopService("stub-528") // compose down may fail here; irrelevant

	svc := findServiceByName(readServicesFromManifest(t, nodeDir), "stub-528")
	if svc == nil {
		t.Fatal("stub-528 missing from manifest after stop")
	}
	if !serviceStartDisabled(*svc) {
		t.Errorf("desired_status = %q, want stopped (TUI stop must be durable)", svc.DesiredStatus)
	}
}

// TestCCStartServiceClearsDurableMarker: the TUI start restores start-on-boot
// by clearing the marker, mirroring liveModuleOps.Start.
func TestCCStartServiceClearsDurableMarker(t *testing.T) {
	nodeDir := writeManifestWithServices(t, []Service{
		{Name: "stub-528", Type: "docker", ComposeFile: filepath.Join("services", "stub-528.yml"), DesiredStatus: "stopped"},
	})

	_ = ccStartService("stub-528") // compose up may fail here; irrelevant

	svc := findServiceByName(readServicesFromManifest(t, nodeDir), "stub-528")
	if svc == nil {
		t.Fatal("stub-528 missing from manifest after start")
	}
	if serviceStartDisabled(*svc) {
		t.Error("TUI start must clear the durable stopped marker")
	}
}

// TestCCRestartServiceClearsDurableMarker: a restart expresses "keep running",
// so it clears the marker too.
func TestCCRestartServiceClearsDurableMarker(t *testing.T) {
	nodeDir := writeManifestWithServices(t, []Service{
		{Name: "stub-528", Type: "docker", ComposeFile: filepath.Join("services", "stub-528.yml"), DesiredStatus: "stopped"},
	})

	_ = ccRestartService("stub-528")

	svc := findServiceByName(readServicesFromManifest(t, nodeDir), "stub-528")
	if svc == nil {
		t.Fatal("stub-528 missing from manifest after restart")
	}
	if serviceStartDisabled(*svc) {
		t.Error("TUI restart must clear the durable stopped marker")
	}
}

// TestSetServiceDesiredStatusRoundTrip covers the cmd-side marker primitive
// used by the TUI and `citadel stop` paths: set, clear, and the loud error on
// an unknown name (a silent no-op would mask a typo'd stop).
func TestSetServiceDesiredStatusRoundTrip(t *testing.T) {
	nodeDir := writeManifestWithServices(t, []Service{
		{Name: "stub-528", Type: "docker", ComposeFile: filepath.Join("services", "stub-528.yml")},
	})

	if err := setServiceDesiredStatus(nodeDir, "stub-528", "stopped"); err != nil {
		t.Fatalf("set: %v", err)
	}
	svc := findServiceByName(readServicesFromManifest(t, nodeDir), "stub-528")
	if svc == nil || !serviceStartDisabled(*svc) {
		t.Fatalf("marker not set: %+v", svc)
	}

	if err := setServiceDesiredStatus(nodeDir, "stub-528", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	svc = findServiceByName(readServicesFromManifest(t, nodeDir), "stub-528")
	if svc == nil || serviceStartDisabled(*svc) {
		t.Fatalf("marker not cleared: %+v", svc)
	}

	if err := setServiceDesiredStatus(nodeDir, "nope", "stopped"); err == nil {
		t.Error("expected error for unknown service, got nil")
	}
}
