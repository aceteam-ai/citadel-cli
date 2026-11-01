package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/containerjobs"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

func TestContainerWorkerDefaultDenyLiveAdvertisementAndRevocation(t *testing.T) {
	dir := t.TempDir()
	containerEnrollmentFixture(t, dir)
	handlers := containerJobHandlersWithReadiness(dir, "linux", true)
	if len(handlers) != len(containerjobs.Types) {
		t.Fatal("incomplete typed family")
	}
	for _, h := range handlers {
		for _, kind := range containerjobs.Types {
			if h.CanHandle(kind) {
				t.Fatal("default host permission broadened")
			}
		}
	}
	p := config.DefaultPermissions()
	containerLimitsFixture(p)
	p.ContainerHost = true
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(NewMockJobSource("synthetic", nil), handlers, RunnerConfig{})
	for _, kind := range containerjobs.Types {
		h := r.findHandler(kind)
		if h == nil {
			t.Fatal("enabled family missing", kind)
		}
		if caps := capabilitiesOf(h, kind); caps.Lane != jobs.LaneHeavy || caps.Deadline != jobs.DeadlineLong {
			t.Fatal("actual-exit custody missing")
		}
	}
	p.ContainerHost = false
	if config.SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	selected := handlers[0]
	_, err := selected.Execute(context.Background(), &Job{Type: containerjobs.Create}, &MockStreamWriter{})
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "container_host_disabled" {
		t.Fatal("revoked handler ran", err)
	}
	for _, kind := range containerjobs.Types {
		if r.findHandler(kind) != nil {
			t.Fatal("revoked capability still advertised")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "container-volumes")); !os.IsNotExist(err) {
		t.Fatal("disabled handler touched runtime storage")
	}
}
func TestContainerWorkerUnreadableAndUnsupportedHostFailClosed(t *testing.T) {
	dir := t.TempDir()
	containerEnrollmentFixture(t, dir)
	p := config.DefaultPermissions()
	containerLimitsFixture(p)
	p.ContainerHost = true
	p.ContainerRuntime = "unreviewed-runtime"
	if config.SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	for _, h := range containerJobHandlersWithReadiness(dir, "linux", true) {
		for _, kind := range containerjobs.Types {
			if h.CanHandle(kind) {
				t.Fatal("unreviewed runtime advertised")
			}
		}
	}
	p.ContainerRuntime = "runsc"
	if config.SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	for _, goos := range []string{"windows", "darwin"} {
		for _, h := range newContainerJobHandlers(dir, goos) {
			for _, kind := range containerjobs.Types {
				if h.CanHandle(kind) {
					t.Fatal("unsupported OS advertised")
				}
			}
		}
	}
	if os.Remove(filepath.Join(dir, "permissions.yaml")) != nil || os.Mkdir(filepath.Join(dir, "permissions.yaml"), 0700) != nil {
		t.Fatal("fixture")
	}
	for _, h := range containerJobHandlersWithReadiness(dir, "linux", true) {
		for _, kind := range containerjobs.Types {
			if h.CanHandle(kind) {
				t.Fatal("unreadable permissions advertised")
			}
		}
	}
}
func TestContainerWorkerTypeInventoryMatchesWireFamily(t *testing.T) {
	var found []string
	for _, kind := range allKnownJobTypes {
		if containerjobs.IsType(kind) {
			found = append(found, kind)
		}
	}
	if !reflect.DeepEqual(found, containerjobs.Types) {
		t.Fatal("heartbeat inventory drift", found)
	}
}

func TestContainerWorkerProxyConfigurationNeverGrantsContainerHost(t *testing.T) {
	dir := t.TempDir()
	containerEnrollmentFixture(t, dir)
	handlers := containerJobHandlersWithReadiness(dir, "linux", true)
	p := config.DefaultPermissions()
	containerLimitsFixture(p)
	p.ContainerEgressProxyURL = "http://host.docker.internal:3128"
	check := func(want bool) {
		t.Helper()
		if err := config.SavePermissions(dir, p); err != nil {
			t.Fatal(err)
		}
		for _, h := range handlers {
			if h.CanHandle(h.(*ContainerJobHandler).kind) != want {
				t.Fatal("proxy policy changed host authority or admitted malformed proxy")
			}
		}
	}
	check(false)
	p.ContainerHost = true
	check(true)
	p.ContainerEgressProxyURL = "http://user:secret@proxy.invalid:3128"
	check(false)
	p.ContainerEgressProxyURL = ""
	check(true)
}

func containerLimitsFixture(p *config.Permissions) {
	p.ContainerMaxQuotaBytes = 10 << 30
	p.ContainerMaxVolumes = 4
	p.ContainerMaxMemoryBytes = 2 << 30
	p.ContainerMaxPids = 256
	p.ContainerMaxCPUs = 1
	p.ContainerMinFreeBytes = 2 << 30
}

func containerEnrollmentFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("device_api_token: synthetic-device\norg_id: "+containerFixtureOrg+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestContainerWorkerRequiresEveryLocalCeilingAndEnrolledOrg(t *testing.T) {
	dir := t.TempDir()
	p := config.DefaultPermissions()
	p.ContainerHost = true
	handlers := containerJobHandlersWithReadiness(dir, "linux", true)
	assertAdvertised := func(want bool) {
		t.Helper()
		if config.SavePermissions(dir, p) != nil {
			t.Fatal("fixture")
		}
		for _, h := range handlers {
			if h.CanHandle(h.(*ContainerJobHandler).kind) != want {
				t.Fatal("missing local budget or enrollment granted a native host")
			}
		}
	}
	assertAdvertised(false)
	containerEnrollmentFixture(t, dir)
	assertAdvertised(false)
	containerLimitsFixture(p)
	assertAdvertised(true)
	for _, change := range []func(*config.Permissions){func(p *config.Permissions) { p.ContainerMaxQuotaBytes = 0 }, func(p *config.Permissions) { p.ContainerMaxVolumes = 0 }, func(p *config.Permissions) { p.ContainerMaxMemoryBytes = 0 }, func(p *config.Permissions) { p.ContainerMaxPids = 0 }, func(p *config.Permissions) { p.ContainerMaxCPUs = 0 }, func(p *config.Permissions) { p.ContainerMinFreeBytes = 0 }} {
		containerLimitsFixture(p)
		change(p)
		assertAdvertised(false)
	}
	containerLimitsFixture(p)
	if os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("device_api_token: synthetic-device\norg_id: invalid\n"), 0600) != nil {
		t.Fatal("fixture")
	}
	assertAdvertised(false)
}
