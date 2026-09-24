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

func TestEffectiveSudoAuditRejectsScopedGrant(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		wantDenied   bool
	}{
		{"no-grants", "Sorry, user citadel may not run sudo on node.\n", &exec.ExitError{}, true},
		{"scoped-nopasswd", "User citadel may run the following commands:\n (root) NOPASSWD: /usr/bin/systemctl\n", nil, false},
		{"scoped-password", "User citadel may run the following commands:\n (root) /usr/bin/systemctl\n", nil, false},
		{"mixed-denial-and-grant", "User citadel may not run sudo normally\nUser citadel may run the following commands: NOPASSWD: /bin/systemctl", &exec.ExitError{}, false},
		{"audit-unavailable", "sudo: a password is required\n", &exec.ExitError{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sudoListDeniesAll(tc.output, tc.err); got != tc.wantDenied {
				t.Fatalf("sudoListDeniesAll=%v, want %v", got, tc.wantDenied)
			}
		})
	}
	if !validPodmanUserMarker([]byte("1001\n"), 0600, 0, "1001") {
		t.Fatal("managed root-owned marker rejected")
	}
	for _, marker := range []struct {
		data []byte
		mode os.FileMode
		uid  uint32
	}{
		{[]byte("1001\n"), 0644, 0},
		{[]byte("1001\n"), 0600, 1001},
		{[]byte("1002\n"), 0600, 0},
		{[]byte("1001\n"), os.ModeSymlink | 0600, 0},
	} {
		if validPodmanUserMarker(marker.data, marker.mode, marker.uid, "1001") {
			t.Fatalf("unsafe marker accepted: %+v", marker)
		}
	}
}

func TestHealthyWorkerRerunStopsBeforeMutations(t *testing.T) {
	// Exercise the entry gate twice against the same fixtures. A first pass
	// reaches provisioning; once the worker is healthy, the second pass must
	// preserve the config bytes and worker PID without invoking any mutator.
	root := t.TempDir()
	manifest := filepath.Join(root, "citadel.yaml")
	pidFile := filepath.Join(root, "worker.pid")
	if err := os.WriteFile(manifest, []byte("node: first-pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte("4172\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	ownerExists := false
	lookup := func(string) (*user.User, error) {
		if !ownerExists {
			return nil, user.UnknownUserError("citadel")
		}
		return &user.User{Username: "citadel", Uid: "1001", HomeDir: root}, nil
	}
	verify := func(*user.User) error { return nil }
	active := func(*user.User) (bool, error) {
		pid, err := os.ReadFile(pidFile)
		return string(pid) == "4172\n", err
	}
	ready, err := classifyExistingPodmanWorker(lookup, verify, active)
	if err != nil || ready {
		t.Fatalf("first pass readiness=%v, err=%v", ready, err)
	}
	mutations++ // the first pass provisions the dedicated account and worker
	ownerExists = true
	// The production entrypoint returns immediately on ready, before the
	// first mutating call. Pin its ordering as well as the fixture state.
	source, err := os.ReadFile("init.go")
	if err != nil {
		t.Fatal(err)
	}
	entry := string(source)
	if guard, mutate := strings.Index(entry, "if ready {"), strings.Index(entry, "ensureNodeIdentity(authServiceURL)"); guard < 0 || mutate < 0 || guard > mutate {
		t.Fatal("healthy-worker return moved after identity mutation")
	}
	beforeManifest, _ := os.ReadFile(manifest)
	beforePID, _ := os.ReadFile(pidFile)
	ready, err = classifyExistingPodmanWorker(lookup, verify, active)
	if err != nil || !ready {
		t.Fatalf("second pass readiness=%v, err=%v", ready, err)
	}
	afterManifest, _ := os.ReadFile(manifest)
	afterPID, _ := os.ReadFile(pidFile)
	if mutations != 1 || string(beforeManifest) != string(afterManifest) || string(beforePID) != string(afterPID) {
		t.Fatal("healthy rerun mutated config, worker PID, or provisioning count")
	}
}

func TestInstallerHealthyRerunSkipsLogAndMutators(t *testing.T) {
	source, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	logPath := filepath.Join(root, "install.log")
	configPath := filepath.Join(root, "citadel.yaml")
	pidPath := filepath.Join(root, "worker.pid")
	mutationPath := filepath.Join(root, "mutation")
	script := strings.Replace(string(source), `LOG_FILE="/var/log/citadel-install.log"`, `LOG_FILE="`+logPath+`"`, 1)
	script = strings.Replace(script, "main \"$@\"", "", 1)
	script += `
original_preflight=$(declare -f preflight)
preflight() { ALREADY_READY=false; }
for step in resolve_authkey detect_gpu install_nvidia_drivers install_podman install_nvidia_toolkit install_node_tools install_citadel_binary prepull_vllm prepull_app_runtimes print_summary log; do
    eval "$step() { :; }"
done
setup_citadel() { printf 'node: original\n' > "` + configPath + `"; }
setup_systemd_service() { :; }
start_worker() { printf '4172\n' > "` + pidPath + `"; printf 'first-pass\n' >> "` + mutationPath + `"; }
main
eval "$original_preflight"
id() { if [ "$1" = -u ]; then echo 0; else return 0; fi; }
verify_service_account() { :; }
existing_rootless_worker() { return 0; }
main
`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("healthy installer rerun failed: %v: %s", err, out)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("healthy rerun created log: %v", err)
	}
	mutations, err := os.ReadFile(mutationPath)
	if err != nil || string(mutations) != "first-pass\n" {
		t.Fatalf("healthy rerun invoked mutator: %v: %q", err, mutations)
	}
	config, _ := os.ReadFile(configPath)
	pid, _ := os.ReadFile(pidPath)
	if string(config) != "node: original\n" || string(pid) != "4172\n" {
		t.Fatal("healthy installer rerun changed config or worker PID")
	}
}

func TestPackerToolkitOnlyCPUArm64SkipsGPUSetup(t *testing.T) {
	source, err := os.ReadFile("../packer/scripts/03-podman.sh")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	if !strings.Contains(code, "if has_nvidia_hardware; then\nfor group in video render") {
		t.Fatal("Packer GPU groups are not gated by hardware")
	}
	if !strings.Contains(code, "if ! has_nvidia_hardware; then\n    exit 0") {
		t.Fatal("CDI boot helper does not skip CPU-only nodes")
	}
}
