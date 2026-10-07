//go:build linux

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
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
	if !strings.Contains(string(worker), `exec.Command("sudo", "-n", "-l", "-U", podmanServiceUser)`) || strings.Contains(string(worker), `"LC_ALL=C", "sudo", "-n", "-l"`) {
		t.Fatal("sudo policy must be queried by root for the dedicated worker")
	}
	installer, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installer), `sudo -n -l -U "$SERVICE_USER"`) || strings.Contains(string(installer), `runuser -u "$SERVICE_USER" -- env LC_ALL=C sudo`) {
		t.Fatal("shell installer must audit the worker sudo policy without authenticating as it")
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
		{"root-query-no-grants", "User citadel is not allowed to run sudo on node.\n", nil, true},
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

func TestPodmanComposeInstallHasJammyFallback(t *testing.T) {
	for _, path := range []string{"../install.sh", "../packer/scripts/03-podman.sh", "init_podman_linux.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code := string(source)
		if !strings.Contains(code, "apt-cache") || !strings.Contains(code, "podman-compose==1.0.6") {
			t.Fatalf("%s lacks the pinned Ubuntu 22.04 podman-compose fallback", path)
		}
	}
	installer, _ := os.ReadFile("../install.sh")
	if strings.Contains(string(installer), "podman podman-compose uidmap") {
		t.Fatal("optional podman-compose package must not make the base dependency transaction fail")
	}
	packer, _ := os.ReadFile("../packer/scripts/03-podman.sh")
	if strings.Contains(string(packer), "podman podman-compose uidmap") {
		t.Fatal("Packer base dependency transaction still requires podman-compose")
	}
}

func TestInstallerParsesMultilineVersionForIdempotentRerun(t *testing.T) {
	source, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	if !strings.Contains(code, "installed_citadel_version()") || !strings.Contains(code, `1s/^Citadel CLI version[[:space:]]*//p`) {
		t.Fatal("installer does not normalize the multiline citadel version output")
	}
	if strings.Contains(code, `current_ver=$("${INSTALL_DIR}/${BINARY_NAME}" version`) {
		t.Fatal("installer still compares the full multiline version output")
	}
}

func TestFreshGPUProvisionCreatesMissingStandardGroups(t *testing.T) {
	installer, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installer), `groupadd --system "$group"`) {
		t.Fatal("fresh installer does not create a missing standard GPU group")
	}
	if !strings.Contains(string(installer), "dbus-user-session git gnupg udev") {
		t.Fatal("fresh installer does not install udevadm before configuring GPU rules")
	}
	if strings.Contains(string(installer), `udevadm control --reload-rules || die`) {
		t.Fatal("fresh installer makes an image-builder udev reload fatal")
	}
	if !strings.Contains(string(installer), "--feature-flag no-additional-gids-for-device-nodes") {
		t.Fatal("fresh installer emits CDI 0.7 additionalGids that Ubuntu 24.04 Podman 4.9 cannot parse")
	}
	packer, err := os.ReadFile("../packer/scripts/03-podman.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packer), `groupadd --system "$group"`) {
		t.Fatal("Packer does not create a missing standard GPU group")
	}
	if !strings.Contains(string(packer), "dbus-user-session udev") {
		t.Fatal("Packer does not install udevadm before configuring GPU rules")
	}
	if !strings.Contains(string(packer), "--feature-flag no-additional-gids-for-device-nodes") {
		t.Fatal("Packer emits CDI 0.7 additionalGids that Ubuntu 24.04 Podman 4.9 cannot parse")
	}
	firstboot, err := os.ReadFile("../packer/scripts/06-firstboot.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstboot), "--feature-flag no-additional-gids-for-device-nodes") {
		t.Fatal("Packer firstboot does not preserve the Podman-4.9-compatible CDI document")
	}
	if strings.Contains(string(firstboot), "/usr/local/libexec/citadel-nvidia-cdi-refresh") {
		t.Fatal("Packer firstboot depends on a refresh helper omitted when the image was built without GPU passthrough")
	}
	if !strings.Contains(string(firstboot), "podman run --rm --pull=never --device nvidia.com/gpu=all") {
		t.Fatal("Packer firstboot starts the worker without a rootless Podman GPU CDI canary")
	}
	worker, err := os.ReadFile("init_podman_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(worker), `provisionCommand("groupadd", "--system", group)`) {
		t.Fatal("Go provisioner does not create a missing standard GPU group")
	}
}

func TestEveryNvidiaCDIGenerationDisablesAdditionalGIDs(t *testing.T) {
	needle := "nvidia-ctk cdi " + "generate"
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".worktrees", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || (!strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sh")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for lineNumber, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, needle) && !strings.Contains(line, "no-additional-gids-for-device-nodes") {
				t.Errorf("%s:%d generates a CDI document incompatible with Podman 4.9", path, lineNumber+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUninstallerMarkerBoundary(t *testing.T) {
	source, err := os.ReadFile("../uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	boundary := strings.SplitN(string(source), "# End independently testable uninstaller marker-boundary functions.", 2)
	if len(boundary) != 2 {
		t.Fatal("uninstaller marker boundary unavailable")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "rootless-worker-user")
	write := func(data string, mode os.FileMode) {
		t.Helper()
		if err := os.RemoveAll(marker); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	owner := strconv.Itoa(os.Getuid())
	check := func(want bool, uid, home, expectedOwner string) {
		t.Helper()
		script := boundary[0] + "\nrootless_worker_marker_valid_at \"$1\" \"$2\" \"$3\" \"$4\"\n"
		cmd := exec.Command("bash", "-c", script, "marker-test", marker, uid, home, expectedOwner)
		if got := cmd.Run() == nil; got != want {
			t.Fatalf("marker safe=%v, want %v (uid=%q home=%q owner=%q)", got, want, uid, home, expectedOwner)
		}
	}

	write("1001\n", 0600)
	check(true, "1001", "/home/citadel", owner)
	check(false, "0", "/home/citadel", owner)
	check(false, "1001", "/home/someone", owner)
	check(false, "1001", "/home/citadel", "99999")
	write("1001 \n", 0600)
	check(false, "1001", "/home/citadel", owner)
	write("1001\n\n", 0600)
	check(false, "1001", "/home/citadel", owner)
	write("1001\n", 0644)
	check(false, "1001", "/home/citadel", owner)
	write("1001\n", 0600)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	check(false, "1001", "/home/citadel", owner)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), marker); err != nil {
		t.Fatal(err)
	}
	check(false, "1001", "/home/citadel", owner)
	linkedParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(dir, linkedParent); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(linkedParent, "rootless-worker-user")
	check(false, "1001", "/home/citadel", owner)
}

func TestPodmanVersionMeetsCDIFloor(t *testing.T) {
	tests := []struct {
		output string
		want   bool
	}{
		{"podman version 3.4.4", false},
		{"podman version 4.0.3", false},
		{"podman version 4.1.0", true},
		{"podman version 4.9.3", true},
		{"podman version 5.4.2", true},
		{"podman version unknown", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := podmanVersionMeetsCDIFloor(tc.output); got != tc.want {
			t.Errorf("podmanVersionMeetsCDIFloor(%q) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

func TestUbuntuJammyDetection(t *testing.T) {
	for _, tc := range []struct {
		data string
		want bool
	}{
		{`ID=ubuntu
VERSION_ID="22.04"
`, true},
		{`ID='ubuntu'
VERSION_ID='24.04'
`, false},
		{`ID=debian
VERSION_ID="12"
`, false},
		{"", false},
	} {
		if got := ubuntuJammy([]byte(tc.data)); got != tc.want {
			t.Errorf("ubuntuJammy(%q) = %v, want %v", tc.data, got, tc.want)
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

func TestInstallerPodmanVersionFloorGate(t *testing.T) {
	source, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Drop the entrypoint so the script only defines functions when sourced.
	script := strings.Replace(string(source), "main \"$@\"", "", 1)
	for _, tc := range []struct {
		version string
		wantOK  bool
	}{
		{"3.4.4", false}, // Ubuntu 22.04 ships this; rootless CDI needs >= 4.1
		{"4.0.3", false}, // major 4 but minor < 1
		{"4.1.0", true},  // exact floor
		{"4.9.3", true},  // Ubuntu 24.04
		{"5.2.0", true},
		{"", false}, // podman missing / unparseable
	} {
		t.Run(tc.version, func(t *testing.T) {
			root := t.TempDir()
			fake := filepath.Join(root, "podman")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'podman version "+tc.version+"\\n'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", script+"\npodman_meets_cdi_floor\n")
			cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"))
			if err := cmd.Run(); (err == nil) != tc.wantOK {
				t.Fatalf("podman_meets_cdi_floor for %q success=%v, want %v", tc.version, err == nil, tc.wantOK)
			}
		})
	}
}

func TestPackerToolkitOnlyCPUArm64SkipsGPUSetup(t *testing.T) {
	source, err := os.ReadFile("../packer/scripts/03-podman.sh")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	start := strings.Index(code, "if command -v nvidia-ctk >/dev/null; then\nif has_nvidia_hardware; then")
	end := strings.Index(code, "\nfi\nfi\npodman --version")
	if start < 0 || end <= start {
		t.Fatal("Packer GPU groups and all CDI artifacts must share the hardware gate")
	}
	root := t.TempDir()
	tool := filepath.Join(root, "nvidia-ctk")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	block := code[start : end+len("\nfi\nfi")]
	for _, path := range []string{"/etc/cdi", "/etc/systemd/system/citadel-nvidia-cdi.service", "/etc/udev/rules.d/70-citadel-nvidia.rules", "/usr/local/libexec"} {
		block = strings.ReplaceAll(block, path, filepath.Join(root, strings.TrimPrefix(path, "/")))
	}
	mutation := filepath.Join(root, "mutation")
	script := `has_nvidia_hardware() { return 1; }
uname() { printf 'aarch64\n'; }
systemctl() { printf 'systemctl\n' >> "` + mutation + `"; }
usermod() { printf 'usermod\n' >> "` + mutation + `"; }
udevadm() { printf 'udevadm\n' >> "` + mutation + `"; }
install() { printf 'install\n' >> "` + mutation + `"; }
` + block
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CPU-only Packer GPU block failed: %v: %s", err, out)
	}
	for _, path := range []string{mutation, filepath.Join(root, "etc/cdi"), filepath.Join(root, "etc/systemd/system/citadel-nvidia-cdi.service"), filepath.Join(root, "usr/local/libexec/citadel-nvidia-cdi-refresh")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("toolkit-only CPU arm64 created GPU mutation/artifact %s: %v", path, err)
		}
	}
}

func TestPackerWorkerMarkerRejectsHostileParentAndLeaf(t *testing.T) {
	source, err := os.ReadFile("../packer/scripts/03-podman.sh")
	if err != nil {
		t.Fatal(err)
	}
	boundary := strings.SplitN(string(source), "# End independently testable marker-boundary functions.", 2)
	if len(boundary) != 2 {
		t.Fatal("Packer worker marker boundary unavailable")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "citadel")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "rootless-worker-user")
	if err := os.WriteFile(marker, []byte("1001\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check := func(path, owner string, want bool) {
		t.Helper()
		script := boundary[0] + "\nworker_marker_safe \"$1\" 1001 \"$2\"\n"
		cmd := exec.Command("bash", "-c", script, "--", path, owner)
		err := cmd.Run()
		if (err == nil) != want {
			t.Fatalf("marker path %s owner %s safe=%v, want %v (err=%v)", path, owner, err == nil, want, err)
		}
	}
	owner := fmt.Sprint(os.Getuid())
	check(marker, owner, true)
	check(marker, "99999", false)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	check(marker, owner, false)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(marker, 0644); err != nil {
		t.Fatal(err)
	}
	check(marker, owner, false)
	if err := os.Chmod(marker, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked-citadel")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	check(filepath.Join(link, "rootless-worker-user"), owner, false)
	leafLink := filepath.Join(dir, "linked-marker")
	if err := os.Symlink(marker, leafLink); err != nil {
		t.Fatal(err)
	}
	check(leafLink, owner, false)
}

func TestFirstbootRetryRefusesActiveWorkerBeforeManagerRestart(t *testing.T) {
	source, err := os.ReadFile("../packer/scripts/06-firstboot.sh")
	if err != nil {
		t.Fatal(err)
	}
	code := string(source)
	start := strings.Index(code, "require_drained_firstboot_worker() {")
	end := strings.Index(code, "\nrequire_drained_firstboot_worker ||")
	if start < 0 || end <= start {
		t.Fatal("firstboot drain guard missing")
	}
	if auth := strings.Index(code, `AUTHKEY=""`); auth < 0 || end > auth {
		t.Fatal("firstboot drain guard must run before authkey or manifest replay")
	}
	if restart := strings.Index(code, `systemctl restart "user@${citadel_uid}.service"`); restart < end {
		t.Fatal("firstboot drain guard follows user-manager restart")
	}
	root := t.TempDir()
	mutation := filepath.Join(root, "manager-restart")
	config := filepath.Join(root, "citadel.yaml")
	pid := filepath.Join(root, "worker.pid")
	if err := os.WriteFile(config, []byte("node: unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pid, []byte("4172\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := `citadel_uid=1001
log() { :; }
systemctl() {
    if [ "$1" = show ]; then printf 'active\n'; else printf 'restart\n' >> "` + mutation + `"; fi
}
as_citadel() {
    if [ "$3" = show-environment ]; then return 0; fi
    if [ "$3" = is-active ] && [ "$5" = citadel-worker.service ]; then return 0; fi
    return 1
}
` + code[start:end] + `
if require_drained_firstboot_worker; then
    printf 'node: changed\n' > "` + config + `"
    printf '0\n' > "` + pid + `"
    systemctl restart user@1001.service
fi
`
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("guard fixture failed: %v: %s", err, out)
	}
	if _, err := os.Stat(mutation); !os.IsNotExist(err) {
		t.Fatalf("active worker retry restarted user manager: %v", err)
	}
	gotConfig, _ := os.ReadFile(config)
	gotPID, _ := os.ReadFile(pid)
	if string(gotConfig) != "node: unchanged\n" || string(gotPID) != "4172\n" {
		t.Fatal("active-worker firstboot retry mutated config or PID")
	}
}
