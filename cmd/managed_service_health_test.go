package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
)

func TestCheckManagedServiceHealthSurfacesMissingAndFailedServices(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"running", "absent", "exited", "stopped", "probe-error"} {
		if err := os.WriteFile(filepath.Join(dir, name+".yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := &CitadelManifest{Services: []Service{
		{Name: "running", ComposeFile: "running.yml"},
		{Name: "absent", ComposeFile: "absent.yml"},
		{Name: "exited", ComposeFile: "exited.yml"},
		{Name: "stopped", ComposeFile: "stopped.yml", DesiredStatus: "stopped"},
		{Name: "missing-file", ComposeFile: "missing.yml"},
		{Name: "probe-error", ComposeFile: "probe-error.yml"},
		{Name: "native", Type: "native"},
	}}
	probe := func(_ string, name string) (compose.ServiceState, error) {
		switch name {
		case "running":
			return compose.ServiceState{State: compose.StateRunning, Running: true}, nil
		case "exited":
			return compose.ServiceState{State: compose.StateStopped, Container: &compose.PSContainer{State: "exited"}}, nil
		case "probe-error":
			return compose.ServiceState{}, errors.New("podman unavailable")
		case "native":
			return compose.ServiceState{State: compose.StateRunning, Running: true, Native: true}, nil
		default:
			return compose.ServiceState{State: compose.StateStopped}, nil
		}
	}

	got := checkManagedServiceHealth(manifest, dir, probe)
	if len(got) != len(manifest.Services) {
		t.Fatalf("checks = %d, want %d", len(got), len(manifest.Services))
	}
	want := []struct {
		state string
		ok    bool
	}{
		{compose.StateRunning, true},
		{"missing", false},
		{"exited", false},
		{compose.StateStopped, true},
		{"missing", false},
		{"error", false},
		{compose.StateRunning, true},
	}
	for i := range want {
		if got[i].State != want[i].state || got[i].OK != want[i].ok {
			t.Errorf("check %d = %+v, want state=%q ok=%v", i, got[i], want[i].state, want[i].ok)
		}
	}
	if managedServicesHealthy(got) {
		t.Fatal("failed and missing configured services must make the service check unhealthy")
	}
}

func TestCheckManagedServiceHealthRejectsRunningDesiredStoppedService(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ollama.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := &CitadelManifest{Services: []Service{{
		Name: "ollama", ComposeFile: "ollama.yml", DesiredStatus: "stopped",
	}}}
	checks := checkManagedServiceHealth(manifest, dir, func(string, string) (compose.ServiceState, error) {
		return compose.ServiceState{State: compose.StateRunning, Running: true, Native: true}, nil
	})
	if len(checks) != 1 || checks[0].OK || checks[0].State != compose.StateRunning {
		t.Fatalf("checks = %+v, want unexpected running service to fail", checks)
	}
}

func TestCheckManagedServiceHealthRejectsPartiallyRunningDesiredStoppedService(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "module.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := &CitadelManifest{Services: []Service{{
		Name: "module", ComposeFile: "module.yml", DesiredStatus: "stopped",
	}}}
	for _, tc := range []struct {
		name string
		ps   string
	}{
		{name: "running_and_exited", ps: `{"ID":"api","Service":"api","State":"running"}
{"ID":"db","Service":"db","State":"exited"}`},
		{name: "running_and_absent", ps: `{"ID":"api","Service":"api","State":"running"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := checkManagedServiceHealth(manifest, dir, func(string, string) (compose.ServiceState, error) {
				return compose.ResolveServiceState([]byte(tc.ps), map[string]bool{"api": true, "db": true}, func() bool { return false }), nil
			})
			if len(checks) != 1 || checks[0].OK || checks[0].State != compose.StatePartial {
				t.Fatalf("checks = %+v, want unhealthy partial state", checks)
			}
			if !strings.Contains(checks[0].Detail, "desired_status: stopped") {
				t.Fatalf("detail = %q, want stopped-target explanation", checks[0].Detail)
			}

			status := dashboardServiceStatus(checks[0])
			if status.Status != "error" || status.Detail != checks[0].Detail {
				t.Fatalf("dashboard status = %+v, want error with exact detail", status)
			}
			raw, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"status":"error"`) || !strings.Contains(string(raw), `"detail":`) {
				t.Fatalf("JSON status = %s, want error + detail", raw)
			}
		})
	}
}
