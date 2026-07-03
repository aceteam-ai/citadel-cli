//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oldWorkerUnit is a pre-#444 install.sh unit: it lacks all four hardening
// directives and only has the old 10s restart (the restart-storm shape).
const oldWorkerUnit = `[Unit]
Description=Citadel Worker - AceTeam Sovereign Compute
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/citadel work
Restart=on-failure
RestartSec=10
Environment=HOME=/root
WorkingDirectory=/root

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=citadel-worker

# Resource limits
LimitNOFILE=65535
LimitNPROC=65535

[Install]
WantedBy=multi-user.target
`

func TestHardenUnitContent_AddsMissingDirectives(t *testing.T) {
	out, changed := hardenUnitContent(oldWorkerUnit)
	if !changed {
		t.Fatal("expected changed=true for a pre-#444 unit")
	}

	for _, want := range []string{
		"StartLimitIntervalSec=300",
		"StartLimitBurst=5",
		"RestartSteps=5",
		"RestartMaxDelaySec=300",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hardened unit missing %q\n---\n%s", want, out)
		}
	}

	// StartLimit* must land in [Unit]; Restart* in [Service].
	unitSection := sectionBody(out, "Unit")
	serviceSection := sectionBody(out, "Service")
	if !strings.Contains(unitSection, "StartLimitIntervalSec=300") ||
		!strings.Contains(unitSection, "StartLimitBurst=5") {
		t.Errorf("StartLimit* not placed in [Unit] section:\n%s", unitSection)
	}
	if !strings.Contains(serviceSection, "RestartSteps=5") ||
		!strings.Contains(serviceSection, "RestartMaxDelaySec=300") {
		t.Errorf("Restart* not placed in [Service] section:\n%s", serviceSection)
	}
	if strings.Contains(unitSection, "RestartSteps") {
		t.Errorf("RestartSteps leaked into [Unit] section:\n%s", unitSection)
	}

	// Original install-specific content must be preserved.
	for _, keep := range []string{
		"LimitNOFILE=65535",
		"WorkingDirectory=/root",
		"SyslogIdentifier=citadel-worker",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("hardening dropped preserved line %q", keep)
		}
	}
}

func TestHardenUnitContent_Idempotent(t *testing.T) {
	once, changed := hardenUnitContent(oldWorkerUnit)
	if !changed {
		t.Fatal("first pass should change the unit")
	}
	twice, changedAgain := hardenUnitContent(once)
	if changedAgain {
		t.Error("second pass reported a change; hardening is not idempotent")
	}
	if twice != once {
		t.Errorf("second pass produced different output:\nFIRST:\n%s\nSECOND:\n%s", once, twice)
	}
}

func TestHardenUnitContent_RewritesStaleValue(t *testing.T) {
	// A unit that has the directive but at a wrong/stale value must be corrected.
	stale := strings.Replace(oldWorkerUnit,
		"RestartSec=10",
		"RestartSec=10\nRestartSteps=99\nRestartMaxDelaySec=45", 1)
	stale = strings.Replace(stale,
		"[Unit]",
		"[Unit]\nStartLimitIntervalSec=1\nStartLimitBurst=1", 1)

	out, changed := hardenUnitContent(stale)
	if !changed {
		t.Fatal("expected stale values to be corrected")
	}
	if strings.Contains(out, "RestartSteps=99") || strings.Contains(out, "StartLimitBurst=1") {
		t.Errorf("stale hardening values not corrected:\n%s", out)
	}
	if !strings.Contains(out, "RestartSteps=5") || !strings.Contains(out, "StartLimitBurst=5") {
		t.Errorf("corrected values missing:\n%s", out)
	}
	// Corrected in place, not duplicated.
	if strings.Count(out, "RestartSteps=") != 1 {
		t.Errorf("RestartSteps appears %d times, want 1:\n%s", strings.Count(out, "RestartSteps="), out)
	}
	if strings.Count(out, "StartLimitBurst=") != 1 {
		t.Errorf("StartLimitBurst appears %d times, want 1:\n%s", strings.Count(out, "StartLimitBurst="), out)
	}
}

func TestHardenUnitContent_AlreadyHardenedUnchanged(t *testing.T) {
	// The current install.sh unit already carries the hardening.
	current := `[Unit]
Description=Citadel Worker - AceTeam Sovereign Compute
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=/usr/local/bin/citadel work
Restart=on-failure
RestartSec=10
RestartSteps=5
RestartMaxDelaySec=300

[Install]
WantedBy=multi-user.target
`
	out, changed := hardenUnitContent(current)
	if changed {
		t.Error("already-hardened unit reported changed=true")
	}
	if out != current {
		t.Errorf("already-hardened unit was modified:\n%s", out)
	}
}

func TestIsCitadelManagedUnit(t *testing.T) {
	if !isCitadelManagedUnit(oldWorkerUnit) {
		t.Error("expected the citadel worker unit to be recognized as managed")
	}
	// An unrelated unit that happens to sit at a candidate path must be refused.
	foreign := `[Unit]
Description=Some other service

[Service]
ExecStart=/usr/bin/other --serve

[Install]
WantedBy=multi-user.target
`
	if isCitadelManagedUnit(foreign) {
		t.Error("foreign unit incorrectly classified as citadel-managed")
	}
}

// TestRematerializeManagedUnits_UserUnit drives the full file path via the user
// unit (no root, no systemctl side effects verified -- daemon-reload failing in
// CI is tolerated and logged). It proves an out-of-date on-disk unit is rewritten
// to the hardened form and that a second run is a no-op.
func TestRematerializeManagedUnits_UserUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	unitPath, err := unitFilePath(true)
	if err != nil {
		t.Fatalf("unitFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a pre-#444 user unit (citadel-managed, missing hardening).
	userUnit := strings.Replace(oldWorkerUnit,
		"Description=Citadel Worker - AceTeam Sovereign Compute",
		"Description=Citadel Node Agent - AceTeam Sovereign Compute", 1)
	if err := os.WriteFile(unitPath, []byte(userUnit), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	rewritten, err := RematerializeManagedUnits(nil)
	if err != nil {
		t.Fatalf("RematerializeManagedUnits: %v", err)
	}
	if len(rewritten) != 1 || rewritten[0] != unitPath {
		t.Fatalf("expected rewrite of %s, got %v", unitPath, rewritten)
	}

	got, _ := os.ReadFile(unitPath)
	for _, want := range []string{"StartLimitIntervalSec=300", "RestartSteps=5", "RestartMaxDelaySec=300"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("rewritten unit missing %q", want)
		}
	}
	// Backup of the prior file exists.
	if _, err := os.Stat(unitPath + ".citadel-bak"); err != nil {
		t.Errorf("expected backup file, got err: %v", err)
	}

	// Second run: unit already current -> no rewrite.
	rewritten2, err := RematerializeManagedUnits(nil)
	if err != nil {
		t.Fatalf("second RematerializeManagedUnits: %v", err)
	}
	if len(rewritten2) != 0 {
		t.Errorf("second run rewrote units (not idempotent): %v", rewritten2)
	}
}

// TestRematerializeManagedUnits_LeavesForeignUnit ensures a non-citadel unit at a
// candidate path is never touched.
func TestRematerializeManagedUnits_LeavesForeignUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	unitPath, err := unitFilePath(true)
	if err != nil {
		t.Fatalf("unitFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	foreign := "[Unit]\nDescription=Not citadel\n\n[Service]\nExecStart=/usr/bin/other\n"
	if err := os.WriteFile(unitPath, []byte(foreign), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rewritten, err := RematerializeManagedUnits(nil)
	if err != nil {
		t.Fatalf("RematerializeManagedUnits: %v", err)
	}
	if len(rewritten) != 0 {
		t.Errorf("foreign unit was rewritten: %v", rewritten)
	}
	got, _ := os.ReadFile(unitPath)
	if string(got) != foreign {
		t.Errorf("foreign unit content changed:\n%s", got)
	}
}

// sectionBody returns the lines of a named [Section] up to the next section
// header, for assertions.
func sectionBody(content, section string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			in = t == "["+section+"]"
			continue
		}
		if in {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
