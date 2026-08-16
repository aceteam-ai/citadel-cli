package platform

import (
	"errors"
	"strings"
	"testing"
)

// probeStub builds engineProbes whose behavior is fully driven by the test, so
// diagnoseEngine is exercised without a real docker/podman/colima install.
// Mirrors internal/catalog/runtime_test.go's probeStub.
func probeStub(found bool, path string, daemonErr error, socket string, kegPath string, kegFound bool) engineProbes {
	return engineProbes{
		lookPath:   func(bin string) (string, bool) { return path, found },
		daemonInfo: func(bin string) error { return daemonErr },
		liveSocket: func() string { return socket },
		brewKeg:    func(formula string) (string, bool) { return kegPath, kegFound },
	}
}

func TestDiagnoseEngine_Healthy(t *testing.T) {
	p := probeStub(true, "/usr/local/bin/docker", nil, "", "", false)
	d := diagnoseEngine("docker", p)

	if !d.Healthy() {
		t.Fatalf("want Healthy(), got %+v", d)
	}
	if d.Diagnose("ollama cannot start") != "" {
		t.Errorf("Diagnose() on a healthy engine = %q, want empty", d.Diagnose(""))
	}
	if got := d.Remediate("darwin"); got != nil {
		t.Errorf("Remediate() on a healthy engine = %v, want nil", got)
	}
}

func TestDiagnoseEngine_CLIMissingNoEvidence(t *testing.T) {
	p := probeStub(false, "", nil, "", "", false)
	d := diagnoseEngine("docker", p)

	if d.Healthy() {
		t.Fatal("want !Healthy()")
	}
	if d.CLIFound {
		t.Error("want CLIFound=false")
	}
	msg := d.Diagnose("ollama cannot start")
	if !strings.Contains(msg, "docker CLI not found on PATH") {
		t.Errorf("Diagnose() = %q, want it to name the CLI and PATH", msg)
	}
	if !strings.Contains(msg, "ollama cannot start") {
		t.Errorf("Diagnose() = %q, want it to include the context", msg)
	}
	if strings.Contains(msg, "exec:") {
		t.Errorf("Diagnose() = %q, must not leak the raw exec error text (citadel-cli#767)", msg)
	}
}

func TestDiagnoseEngine_CLIMissingRuntimeRunning(t *testing.T) {
	sock := "/Users/jason/.colima/default/docker.sock"
	p := probeStub(false, "", nil, sock, "", false)
	d := diagnoseEngine("docker", p)

	if d.RuntimeSocket != sock {
		t.Errorf("RuntimeSocket = %q, want %q", d.RuntimeSocket, sock)
	}
	msg := d.Diagnose("")
	if !strings.Contains(msg, sock) {
		t.Errorf("Diagnose() = %q, want it to cite the socket path", msg)
	}
	if !strings.Contains(msg, "appears to be running") {
		t.Errorf("Diagnose() = %q, want a conservative phrasing (a socket is not proof of a healthy daemon)", msg)
	}
}

// TestDiagnoseEngine_CLIMissingHomebrewUnlinked pins the exact citadel-cli#767
// reproduction: docker installed via Homebrew but not linked, while colima is
// running underneath it. The keg finding must take priority over (and does not
// need) the socket finding in the message, and the remediation must be the
// precise unblocking command, not a reinstall.
func TestDiagnoseEngine_CLIMissingHomebrewUnlinked(t *testing.T) {
	keg := "/opt/homebrew/opt/docker/bin/docker"
	p := probeStub(false, "", nil, "/Users/jason/.colima/default/docker.sock", keg, true)
	d := diagnoseEngine("docker", p)

	if d.HomebrewKegPath != keg {
		t.Fatalf("HomebrewKegPath = %q, want %q", d.HomebrewKegPath, keg)
	}
	msg := d.Diagnose("ollama cannot start")
	if !strings.Contains(msg, "not linked") || !strings.Contains(msg, keg) {
		t.Errorf("Diagnose() = %q, want it to explain the keg is unlinked and name its path", msg)
	}

	remedy := d.Remediate("darwin")
	if len(remedy) != 1 || remedy[0] != "brew link --overwrite docker" {
		t.Errorf("Remediate(darwin) = %v, want exactly [\"brew link --overwrite docker\"]", remedy)
	}
}

func TestDiagnoseEngine_DaemonUnreachable(t *testing.T) {
	daemonErr := errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	p := probeStub(true, "/usr/local/bin/docker", daemonErr, "", "", false)
	d := diagnoseEngine("docker", p)

	if !d.CLIFound {
		t.Fatal("want CLIFound=true")
	}
	if d.Healthy() {
		t.Fatal("want !Healthy() when the daemon is unreachable")
	}
	msg := d.Diagnose("ollama cannot start")
	if !strings.Contains(msg, "daemon is unreachable") {
		t.Errorf("Diagnose() = %q, want it to name the daemon (not the CLI) as the problem", msg)
	}
}

func TestEngineDiagnosis_RemediatePerPlatform(t *testing.T) {
	tests := []struct {
		name     string
		diag     EngineDiagnosis
		goos     string
		wantSubs []string
	}{
		{
			name:     "darwin CLI missing, no homebrew evidence",
			diag:     EngineDiagnosis{Bin: "docker"},
			goos:     "darwin",
			wantSubs: []string{"brew install docker", "colima", "Docker Desktop"},
		},
		{
			name:     "linux CLI missing",
			diag:     EngineDiagnosis{Bin: "docker"},
			goos:     "linux",
			wantSubs: []string{"get.docker.com", "systemctl start docker"},
		},
		{
			name:     "windows CLI missing",
			diag:     EngineDiagnosis{Bin: "docker"},
			goos:     "windows",
			wantSubs: []string{"winget install Docker.DockerDesktop"},
		},
		{
			name:     "darwin daemon unreachable",
			diag:     EngineDiagnosis{Bin: "docker", CLIFound: true, DaemonErr: "boom"},
			goos:     "darwin",
			wantSubs: []string{"colima start"},
		},
		{
			name:     "linux daemon unreachable",
			diag:     EngineDiagnosis{Bin: "docker", CLIFound: true, DaemonErr: "boom"},
			goos:     "linux",
			wantSubs: []string{"systemctl start docker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(tt.diag.Remediate(tt.goos), "\n")
			for _, sub := range tt.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("Remediate(%s) = %q, want it to contain %q", tt.goos, got, sub)
				}
			}
		})
	}
}

// TestDiagnoseEngine_PodmanBinNotHardcodedDocker guards against re-introducing
// the "advertises what it can't deliver" bug family (#654/#649): a node whose
// resolved container runtime is podman must be diagnosed AS podman, not have
// "docker" baked into the message.
func TestDiagnoseEngine_PodmanBinNotHardcodedDocker(t *testing.T) {
	p := probeStub(false, "", nil, "", "", false)
	d := diagnoseEngine("podman", p)

	msg := d.Diagnose("")
	if strings.Contains(msg, "docker") {
		t.Errorf("Diagnose() for bin=podman = %q, must not mention docker", msg)
	}
	if !strings.Contains(msg, "podman") {
		t.Errorf("Diagnose() for bin=podman = %q, want it to name podman", msg)
	}
	remedy := strings.Join(d.Remediate("darwin"), "\n")
	if !strings.Contains(remedy, "brew install podman") {
		t.Errorf("Remediate(darwin) for bin=podman = %q, want it to suggest installing podman", remedy)
	}
}

// TestHostBrewKeg_NonDarwinAlwaysFalse pins that the Homebrew-specific probe
// never runs (and never shells out to `brew`) off macOS.
func TestHostBrewKeg_NonDarwinAlwaysFalse(t *testing.T) {
	if OS() == "darwin" {
		t.Skip("darwin-exempt: this pins the non-darwin short-circuit")
	}
	if path, found := hostBrewKeg("docker"); found {
		t.Errorf("hostBrewKeg on %s = (%q, true), want (_, false)", OS(), path)
	}
}

// TestDiagnoseEngine_RealHostDoesNotPanic exercises the real (non-stubbed)
// probes end to end. It makes no assertion about the outcome -- whether docker
// happens to be installed on the machine running `go test` is not something
// this suite controls -- only that DiagnoseEngine completes without panicking
// and returns internally consistent fields.
func TestDiagnoseEngine_RealHostDoesNotPanic(t *testing.T) {
	d := DiagnoseEngine("docker")
	if !d.CLIFound && d.DaemonReachable {
		t.Errorf("impossible combination: CLIFound=false but DaemonReachable=true: %+v", d)
	}
	if d.CLIFound && (d.RuntimeSocket != "" || d.HomebrewKegPath != "") {
		t.Errorf("RuntimeSocket/HomebrewKegPath are CLI-missing-only diagnostics, got %+v", d)
	}
}
