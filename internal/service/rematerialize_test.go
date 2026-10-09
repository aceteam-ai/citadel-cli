//go:build linux

package service

import (
	"fmt"
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

// fakeSystemctl records runCmd invocations so tests can assert which
// daemon-reload (system vs --user) was requested without shelling out to the
// real systemctl -- critical because the build host is itself a live citadel
// node running a systemd user unit (see CLAUDE.md hermeticity notes).
type fakeSystemctl struct {
	calls [][]string
}

func (f *fakeSystemctl) run(name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

// writeTempUnit writes content to a fresh file in t.TempDir() and returns a
// candidate for it, so the injectable core operates entirely under a tempdir --
// never a real /etc or ~/.config unit.
func writeTempUnit(t *testing.T, content string, userMode bool) managedUnitCandidate {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "citadel-worker.service")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp unit: %v", err)
	}
	return managedUnitCandidate{path: path, userMode: userMode}
}

// TestDecideUnitRefresh pins the pure per-candidate verdict, including the
// root/non-root branch for system units (euid injected, no privilege drop).
func TestDecideUnitRefresh(t *testing.T) {
	alreadyHardened, _ := hardenUnitContent(oldWorkerUnit)
	foreign := "[Unit]\nDescription=Not citadel\n\n[Service]\nExecStart=/usr/bin/other\n"

	cases := []struct {
		name     string
		content  string
		userMode bool
		euid     int
		want     unitRefreshDecision
	}{
		{"system unit, drift, root -> rewrite", oldWorkerUnit, false, 0, refreshRewrite},
		{"system unit, drift, non-root -> needs root", oldWorkerUnit, false, 1000, refreshNeedsRoot},
		{"user unit, drift, non-root -> rewrite", oldWorkerUnit, true, 1000, refreshRewrite},
		{"already hardened -> no change", alreadyHardened, false, 0, refreshNoChange},
		{"foreign unit -> not managed", foreign, false, 0, refreshNotManaged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hardened, got := decideUnitRefresh(tc.content, tc.userMode, tc.euid)
			if got != tc.want {
				t.Fatalf("decision = %d, want %d", got, tc.want)
			}
			// On a rewrite/needs-root verdict the hardened content must carry the
			// #444 directives; otherwise it must be empty.
			if tc.want == refreshRewrite || tc.want == refreshNeedsRoot {
				for _, d := range []string{"StartLimitIntervalSec=300", "StartLimitBurst=5", "RestartSteps=5", "RestartMaxDelaySec=300"} {
					if !strings.Contains(hardened, d) {
						t.Errorf("hardened content missing %q", d)
					}
				}
			} else if hardened != "" {
				t.Errorf("expected empty hardened content for decision %d, got %q", got, hardened)
			}
		})
	}
}

// TestRematerializeManagedUnits_SystemUnitAsRoot drives the full loop over a
// tempdir SYSTEM unit with euid=0: the unit is rewritten with the #444
// directives, backed up, and a SYSTEM daemon-reload (not --user) is requested
// via the injected runner. A second run over the now-current unit is a clean
// no-op (no rewrite, no reload).
func TestRematerializeManagedUnits_SystemUnitAsRoot(t *testing.T) {
	cand := writeTempUnit(t, oldWorkerUnit, false /* system */)
	fake := &fakeSystemctl{}

	rewritten, err := rematerializeManagedUnits([]managedUnitCandidate{cand}, 0 /* root */, fake.run, nil)
	if err != nil {
		t.Fatalf("rematerializeManagedUnits: %v", err)
	}
	if len(rewritten) != 1 || rewritten[0] != cand.path {
		t.Fatalf("expected rewrite of %s, got %v", cand.path, rewritten)
	}

	got, _ := os.ReadFile(cand.path)
	for _, want := range []string{"StartLimitIntervalSec=300", "StartLimitBurst=5", "RestartSteps=5", "RestartMaxDelaySec=300"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("rewritten unit missing %q\n%s", want, got)
		}
	}
	if _, err := os.Stat(cand.path + ".citadel-bak"); err != nil {
		t.Errorf("expected backup file, got err: %v", err)
	}
	// A system unit must trigger a SYSTEM daemon-reload, never --user.
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly one systemctl call, got %v", fake.calls)
	}
	if got := strings.Join(fake.calls[0], " "); got != "systemctl daemon-reload" {
		t.Fatalf("expected `systemctl daemon-reload`, got %q", got)
	}

	// Second run: already current -> no rewrite, no reload churn.
	fake2 := &fakeSystemctl{}
	rewritten2, err := rematerializeManagedUnits([]managedUnitCandidate{cand}, 0, fake2.run, nil)
	if err != nil {
		t.Fatalf("second rematerializeManagedUnits: %v", err)
	}
	if len(rewritten2) != 0 {
		t.Errorf("second run rewrote units (not idempotent): %v", rewritten2)
	}
	if len(fake2.calls) != 0 {
		t.Errorf("second run issued a daemon-reload on an unchanged unit: %v", fake2.calls)
	}
}

// TestRematerializeManagedUnits_SystemUnitNonRoot proves the non-root path:
// a system unit with drift is NOT rewritten (no privilege), its content is left
// untouched, no daemon-reload is attempted, and the logged remediation is the
// corrected, secure_path-proof form -- never a bare `sudo citadel`.
func TestRematerializeManagedUnits_SystemUnitNonRoot(t *testing.T) {
	cand := writeTempUnit(t, oldWorkerUnit, false /* system */)
	fake := &fakeSystemctl{}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rewritten, err := rematerializeManagedUnits([]managedUnitCandidate{cand}, 1000 /* non-root */, fake.run, logf)
	if err != nil {
		t.Fatalf("rematerializeManagedUnits: %v", err)
	}
	if len(rewritten) != 0 {
		t.Fatalf("non-root run rewrote a system unit: %v", rewritten)
	}
	if got, _ := os.ReadFile(cand.path); string(got) != oldWorkerUnit {
		t.Errorf("system unit content changed on a non-root run:\n%s", got)
	}
	if len(fake.calls) != 0 {
		t.Errorf("non-root run attempted a daemon-reload: %v", fake.calls)
	}
	if _, err := os.Stat(cand.path + ".citadel-bak"); err == nil {
		t.Errorf("non-root run wrote a backup file (should not touch the unit at all)")
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "requires root") {
		t.Fatalf("expected a needs-root remediation log, got:\n%s", joined)
	}
	if strings.Contains(joined, "sudo citadel ") {
		t.Fatalf("remediation uses a bare `sudo citadel` that secure_path breaks:\n%s", joined)
	}
	if !strings.Contains(joined, "service refresh-unit") {
		t.Fatalf("remediation should point at the network-free `service refresh-unit`:\n%s", joined)
	}
}

// TestRematerializeManagedUnits_LeavesForeignUnit ensures a non-citadel unit at
// a candidate path is never touched and never triggers a reload.
func TestRematerializeManagedUnits_LeavesForeignUnit(t *testing.T) {
	foreign := "[Unit]\nDescription=Not citadel\n\n[Service]\nExecStart=/usr/bin/other\n"
	cand := writeTempUnit(t, foreign, true)
	fake := &fakeSystemctl{}

	rewritten, err := rematerializeManagedUnits([]managedUnitCandidate{cand}, 0, fake.run, nil)
	if err != nil {
		t.Fatalf("rematerializeManagedUnits: %v", err)
	}
	if len(rewritten) != 0 {
		t.Errorf("foreign unit was rewritten: %v", rewritten)
	}
	if got, _ := os.ReadFile(cand.path); string(got) != foreign {
		t.Errorf("foreign unit content changed:\n%s", got)
	}
	if len(fake.calls) != 0 {
		t.Errorf("foreign unit triggered a daemon-reload: %v", fake.calls)
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
