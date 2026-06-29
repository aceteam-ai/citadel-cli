package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// hasRisk reports whether risks contains a finding with the given severity whose
// directive contains the substring.
func hasRisk(risks []ComposeRisk, sev Severity, directiveSubstr string) bool {
	for _, r := range risks {
		if r.Severity == sev && contains(r.Directive, directiveSubstr) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestScanComposeRisks_Privileged(t *testing.T) {
	compose := `services:
  app:
    image: x
    privileged: true
`
	risks := ScanComposeRisks(compose)
	if !hasRisk(risks, SeverityCritical, "privileged") {
		t.Fatalf("expected Critical privileged risk, got %+v", risks)
	}
}

func TestScanComposeRisks_DockerSocket(t *testing.T) {
	for _, sock := range []string{"/var/run/docker.sock:/var/run/docker.sock", "/run/docker.sock:/run/docker.sock:ro"} {
		compose := "services:\n  app:\n    image: x\n    volumes:\n      - " + sock + "\n"
		risks := ScanComposeRisks(compose)
		if !hasRisk(risks, SeverityCritical, "docker.sock") {
			t.Errorf("expected Critical docker.sock risk for %q, got %+v", sock, risks)
		}
	}
}

func TestScanComposeRisks_CapAddAll(t *testing.T) {
	compose := `services:
  app:
    image: x
    cap_add:
      - ALL
`
	risks := ScanComposeRisks(compose)
	if !hasRisk(risks, SeverityCritical, "cap_add: ALL") {
		t.Fatalf("expected Critical cap_add ALL, got %+v", risks)
	}
}

func TestScanComposeRisks_CapAddSysAdminInline(t *testing.T) {
	compose := "services:\n  app:\n    image: x\n    cap_add: [SYS_ADMIN]\n"
	risks := ScanComposeRisks(compose)
	if !hasRisk(risks, SeverityCritical, "SYS_ADMIN") {
		t.Fatalf("expected Critical cap_add SYS_ADMIN (inline), got %+v", risks)
	}
}

func TestScanComposeRisks_CapAddBenign(t *testing.T) {
	// A non-dangerous cap should not be flagged.
	compose := `services:
  app:
    image: x
    cap_add:
      - NET_ADMIN
`
	risks := ScanComposeRisks(compose)
	if len(criticalRisks(risks)) != 0 {
		t.Errorf("NET_ADMIN should not be Critical, got %+v", risks)
	}
}

func TestScanComposeRisks_HostNamespaces(t *testing.T) {
	cases := map[string]string{
		"network_mode": "    network_mode: host",
		"pid":          "    pid: host",
		"ipc":          "    ipc: host",
	}
	for name, line := range cases {
		compose := "services:\n  app:\n    image: x\n" + line + "\n"
		risks := ScanComposeRisks(compose)
		if !hasRisk(risks, SeverityHigh, name) {
			t.Errorf("expected High %s risk, got %+v", name, risks)
		}
	}
}

func TestScanComposeRisks_SensitiveBindMounts(t *testing.T) {
	for _, host := range []string{"/", "/etc", "/etc/passwd", "/root", "/home", "/var/run", "~", "~/secrets"} {
		compose := "services:\n  app:\n    image: x\n    volumes:\n      - " + host + ":/mnt\n"
		risks := ScanComposeRisks(compose)
		if !hasRisk(risks, SeverityHigh, "bind mount") {
			t.Errorf("expected High bind-mount risk for host %q, got %+v", host, risks)
		}
	}
}

func TestScanComposeRisks_NamedVolumeNotFlagged(t *testing.T) {
	// A named volume must NOT be flagged as a host bind mount.
	compose := `services:
  app:
    image: x
    volumes:
      - pgdata:/var/lib/postgresql/data
`
	risks := ScanComposeRisks(compose)
	if hasRisk(risks, SeverityHigh, "bind mount") {
		t.Errorf("named volume should not be a bind-mount risk, got %+v", risks)
	}
}

func TestScanComposeRisks_BoundaryNotSubstring(t *testing.T) {
	// "/etcetera" must not match "/etc".
	compose := "services:\n  app:\n    image: x\n    volumes:\n      - /etcetera/data:/mnt\n"
	risks := ScanComposeRisks(compose)
	if hasRisk(risks, SeverityHigh, "bind mount") {
		t.Errorf("/etcetera should not match /etc, got %+v", risks)
	}
}

func TestScanComposeRisks_Devices(t *testing.T) {
	compose := "services:\n  app:\n    image: x\n    devices:\n      - /dev/snd:/dev/snd\n"
	risks := ScanComposeRisks(compose)
	if !hasRisk(risks, SeverityHigh, "devices") {
		t.Errorf("expected High devices risk, got %+v", risks)
	}
}

func TestScanComposeRisks_Clean(t *testing.T) {
	compose := `services:
  app:
    image: ghcr.io/acme/app:latest
    ports:
      - "8080:8080"
    volumes:
      - appdata:/data
    environment:
      - FOO=bar
volumes:
  appdata:
`
	risks := ScanComposeRisks(compose)
	if len(risks) != 0 {
		t.Errorf("expected no risks for a clean compose, got %+v", risks)
	}
}

func TestCriticalRisks(t *testing.T) {
	risks := []ComposeRisk{
		{Severity: SeverityHigh, Directive: "devices"},
		{Severity: SeverityCritical, Directive: "privileged: true"},
		{Severity: SeverityCritical, Directive: "docker.sock mount"},
	}
	crit := criticalRisks(risks)
	if len(crit) != 2 {
		t.Fatalf("expected 2 critical, got %d", len(crit))
	}
	dirs := criticalDirectives(risks)
	if len(dirs) != 2 || dirs[0] != "privileged: true" {
		t.Errorf("unexpected critical directives: %v", dirs)
	}
}

func TestInstallFromManifest_PrivilegedGate(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	// A compose with a Critical risk (privileged: true), no ports/container_name.
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: ghcr.io/x/y:latest\n    privileged: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := &ServiceManifest{Name: "risky"}
	servicesDir := filepath.Join(dir, "services")

	// allowPrivileged=false must REFUSE even though interactive=false.
	if _, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, false, false, false); err == nil {
		t.Fatal("expected refusal for Critical compose without allowPrivileged")
	}

	// allowPrivileged=true proceeds past the gate (install succeeds: no ports,
	// no required config, compose copied).
	res, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, true, false, false)
	if err != nil {
		t.Fatalf("expected install to proceed with allowPrivileged=true, got %v", err)
	}
	if res == nil || res.Name != "risky" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestInstallFromManifest_CleanComposeNoGate(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: ghcr.io/x/y:latest\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := &ServiceManifest{Name: "clean"}
	servicesDir := filepath.Join(dir, "services")
	// A clean compose installs fine even with allowPrivileged=false.
	if _, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, false, false, false); err != nil {
		t.Fatalf("clean compose should not be gated, got %v", err)
	}
}
