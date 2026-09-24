//go:build linux

package cmd

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextSubIDRangeAvoidsBothMaps(t *testing.T) {
	uid := []byte("alice:100000:65536\n")
	gid := []byte("bob:165536:65536\n")
	if got := nextSubIDRange(uid, gid); got != 262144 {
		t.Fatalf("next range = %d, want 262144", got)
	}
	if hasSubIDRange(uid, "bob") || !hasSubIDRange(uid, "alice") {
		t.Fatal("subordinate ID ownership was not matched exactly")
	}
	if hasSubIDRange([]byte("alice:100000:1000\n"), "alice") {
		t.Fatal("short mapping must not count as the rootless range")
	}
}

func TestProvisionedUserCommandCarriesRootlessSocket(t *testing.T) {
	owner, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runAsUser(owner.Username, "true").Args, " ")
	for _, required := range []string{
		"XDG_RUNTIME_DIR=/run/user/" + owner.Uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + owner.Uid + "/bus",
		"-u SUDO_USER",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("provision command lacks %q: %s", required, args)
		}
	}
}

func TestLegacyWorkerGuardPreservesStateBeforeAnyMutation(t *testing.T) {
	for _, unit := range []string{"citadel-worker.service", "citadel.service"} {
		t.Run(unit, func(t *testing.T) {
			root := t.TempDir()
			legacy := filepath.Join(root, unit)
			marker := filepath.Join(root, "mutation-marker")
			if err := os.WriteFile(legacy, []byte("old Docker worker\n"), 0600); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(root, "citadel-worker.service"), filepath.Join(root, "citadel.service")}
			if err := rejectLegacySystemWorkerAt(paths, os.Lstat); err == nil {
				t.Fatal("legacy worker accepted")
			} else if !strings.Contains(err.Error(), unit) {
				t.Fatalf("wrong unit in error: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("guard mutated marker: %v", err)
			}
			data, err := os.ReadFile(legacy)
			if err != nil || string(data) != "old Docker worker\n" {
				t.Fatalf("legacy unit changed: %q: %v", data, err)
			}
		})
	}
}

func TestInstallerGuardRunsBeforeLogOrHostMutation(t *testing.T) {
	source, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	preflight := strings.SplitN(string(source), "# Authkey", 2)[0]
	for _, unit := range []string{"citadel-worker.service", "citadel.service"} {
		t.Run(unit, func(t *testing.T) {
			root := t.TempDir()
			legacy := filepath.Join(root, unit)
			log := filepath.Join(root, "install.log")
			if err := os.WriteFile(legacy, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(preflight, "/etc/systemd/system/citadel-worker.service", filepath.Join(root, "citadel-worker.service"))
			script = strings.ReplaceAll(script, "/etc/systemd/system/citadel.service", filepath.Join(root, "citadel.service"))
			script = strings.Replace(script, `LOG_FILE="/var/log/citadel-install.log"`, `LOG_FILE="`+log+`"`, 1)
			script += "\npreflight\n"
			out, err := exec.Command("bash", "-c", script).CombinedOutput()
			if err == nil || !strings.Contains(string(out), legacy) {
				t.Fatalf("guard did not reject %s: %v: %s", unit, err, out)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("installer created log before guard: %v", err)
			}
		})
	}
}

func TestJetsonDetectionWithoutPCIOrNvidiaSMI(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"l4t-release", map[string]string{"release": "# R36 (release), REVISION: 4.3"}, true},
		{"device-tree", map[string]string{"model": "NVIDIA Jetson AGX Orin\x00"}, true},
		{"cpu-only-arm64", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := func(path string) ([]byte, error) {
				if value, ok := tc.files[path]; ok {
					return []byte(value), nil
				}
				return nil, os.ErrNotExist
			}
			if got := isJetsonAt("release", []string{"model"}, read); got != tc.want {
				t.Fatalf("isJetsonAt=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstallerJetsonProbeIsHermeticWithoutPCIOrSMI(t *testing.T) {
	source, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(source), "# GPU detection", 2)
	if len(parts) != 2 {
		t.Fatal("GPU detection section missing")
	}
	probe := strings.SplitN(parts[1], "# NVIDIA drivers", 2)[0]
	for _, tc := range []struct {
		name, release, model string
		want                 bool
	}{
		{"l4t", "# R36 (release)", "", true},
		{"device-tree", "", "NVIDIA Jetson AGX Orin", true},
		{"cpu-arm64", "", "Generic ARM Server", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			release := filepath.Join(root, "nv_tegra_release")
			model := filepath.Join(root, "model")
			if tc.release != "" {
				if err := os.WriteFile(release, []byte(tc.release), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.model != "" {
				if err := os.WriteFile(model, []byte(tc.model), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(probe, "/etc/nv_tegra_release", release)
			script = strings.ReplaceAll(script, "/proc/device-tree/model", model)
			script = strings.ReplaceAll(script, "/sys/firmware/devicetree/base/model", filepath.Join(root, "other-model"))
			cmd := exec.Command("bash", "-c", script+"\nis_jetson\n")
			if err := cmd.Run(); (err == nil) != tc.want {
				t.Fatalf("is_jetson success=%v, want %v", err == nil, tc.want)
			}
		})
	}
}

func TestRootlessRequirementsAreExact(t *testing.T) {
	if err := checkRequiredWords("delegation", "cpu memory pids io", []string{"cpu", "memory", "pids"}); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"cpu memory", "cpu memory pid", "cpu pids"} {
		if err := checkRequiredWords("delegation", data, []string{"cpu", "memory", "pids"}); err == nil {
			t.Fatalf("accepted insufficient controllers %q", data)
		}
	}
	if strings.Contains(nvidiaCDIRefreshScript, "nvidia-smi") || strings.Contains(nvidiaCDIRefreshScript, "/dev/nvidiactl") || !strings.Contains(nvidiaCDIRefreshScript, "nvidia-ctk cdi list") {
		t.Fatal("CDI refresh lost Jetson-safe readiness checks")
	}
}

func TestProvisionDoesNotGrantWorkerSudoOrUseInvokingOwner(t *testing.T) {
	source, err := os.ReadFile("init.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	if strings.Contains(code, "NOPASSWD") || strings.Contains(code, `AddUserToGroup(originalUser, "sudo")`) {
		t.Fatal("init must never grant passwordless sudo to the worker")
	}
	guard := strings.Index(code, "prepareLinuxPodmanProvision()")
	identity := strings.Index(code, "ensureNodeIdentity(authServiceURL)")
	auth := strings.Index(code, "nexus.GetNetworkChoice(authkey)")
	if guard < 0 || identity < 0 || auth < 0 || guard > identity || guard > auth {
		t.Fatal("legacy worker gate must precede identity and auth mutations")
	}
	worker, err := os.ReadFile("init_podman_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(worker), "name := platform.GetSudoUser()") {
		t.Fatal("rootless owner regressed to invoking sudo user")
	}
	packer, err := os.ReadFile("../packer/deploy-to-proxmox.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(packer), "docker,systemd-journal") {
		t.Fatal("Packer deploy must not add docker group")
	}
}
