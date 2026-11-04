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

func TestRefreshPreservesPrivilegeSeparatedWorker(t *testing.T) {
	before := oldWorkerUnit
	before = strings.Replace(before, "[Service]\n", "[Service]\nUser=dedicated-fixture\nGroup=dedicated-fixture\nEnvironment=CITADEL_NO_AUTO_UPDATE=1\nEnvironment=KEEP=unchanged\nProtectSystem=strict\nReadWritePaths=/fixture/state\n", 1)
	got, decision := decideUnitRefresh(before, false, 0)
	if decision != refreshRewrite {
		t.Fatalf("restart hardening not applied: %v", decision)
	}
	for _, line := range []string{"User=dedicated-fixture", "Group=dedicated-fixture", "Environment=CITADEL_NO_AUTO_UPDATE=1", "Environment=KEEP=unchanged", "ProtectSystem=strict", "ReadWritePaths=/fixture/state"} {
		if strings.Count(got, line+"\n") != 1 {
			t.Fatalf("refresh changed privilege-separation directive %q: %s", line, got)
		}
	}
	if strings.Contains(got, "ReadWritePaths=-/etc/citadel") || strings.Contains(got, "ReadWritePaths=/usr/local/bin") {
		t.Fatalf("refresh expanded worker write authority: %s", got)
	}
	again, decision := decideUnitRefresh(got, false, 0)
	if decision != refreshNoChange || again != "" {
		t.Fatal("privilege-separated refresh not byte-idempotent")
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

// All new repair fixtures start with #444 already applied. A new path repair
// cannot accidentally pass because unrelated restart hardening caused a write.
func oldRootSandboxUnit() string {
	content := strings.Replace(oldWorkerUnit, "[Unit]\n", "[Unit]\nStartLimitIntervalSec=300\nStartLimitBurst=5\n", 1)
	return strings.Replace(content, "[Service]\n", "[Service]\nRestartSteps=5\nRestartMaxDelaySec=300\nUser=root\nProtectSystem=strict\nReadWritePaths=/root/.citadel-cli /root/citadel-node\n", 1)
}

func TestRootSandboxFixtureAlreadyCarriesRestartHardening(t *testing.T) {
	before := oldRootSandboxUnit()
	got, changed := hardenUnitContent(before)
	if changed || got != before {
		t.Fatal("root config-path fixture unexpectedly needed restart hardening")
	}
}

func TestRootConfigWritePathRefusesContinuationThatHidesAServiceHeader(t *testing.T) {
	before := strings.Replace(oldRootSandboxUnit(), "User=root", "User=operator", 1)
	before = strings.Replace(before, "[Install]\n", "[Unit]\nDescription=Citadel continued \\\n[Service]\nUser=root\n[Service]\nEnvironment=KEEP=unchanged\n\n[Install]\n", 1)
	// The physical first Service header after Description is continued text,
	// not a logical header. Its User=root is in Unit, leaving the actual
	// Service owner as operator. A physical-line scanner must not infer root.
	got, changed := rootConfigWritePathContent(before, false)
	if changed || got != before {
		t.Fatal("outside-Service continuation authorized a new config path")
	}
	got, decision := decideUnitRefresh(before, false, 0)
	if decision != refreshNoChange || got != "" {
		t.Fatalf("already-hardened ambiguous unit was rewritten: %d\n%s", decision, got)
	}
}

func TestUnitRefreshChecksOriginalIdentityBeforeRestartNormalization(t *testing.T) {
	before := strings.Replace(oldRootSandboxUnit(), "User=root", "User=operator", 1)
	before = strings.Replace(before, "StartLimitBurst=5\n", "", 1)
	before = strings.Replace(before, "[Install]\n", "[Unit]\nStartLimitBurst=99\\\n[Service]\nUser=root\n[Service]\nEnvironment=KEEP=unchanged\n\n[Install]\n", 1)
	// The legacy hardener rewrites the stale StartLimitBurst value, removing
	// its continuation. That must not retroactively authorize a new path grant
	// from a physical User line that was not in the original logical Service.
	got, decision := decideUnitRefresh(before, false, 0)
	if decision != refreshRewrite || !strings.Contains(got, "StartLimitBurst=5\n") {
		t.Fatalf("legacy restart repair was suppressed: %d\n%s", decision, got)
	}
	if strings.Contains(got, "ReadWritePaths=-/etc/citadel") {
		t.Fatal("restart normalization laundered uncertain ownership into a path grant")
	}
}

func TestRootConfigWritePathAllowsCommentBackslashes(t *testing.T) {
	before := strings.Replace(oldRootSandboxUnit(), "[Unit]\n", "[Unit]\n# operator note \\\n", 1)
	before = strings.Replace(before, "[Service]\n", "[Service]\n; operator note \\\n", 1)
	want := strings.Replace(before, "LimitNPROC=65535\n", "LimitNPROC=65535\nReadWritePaths=-/etc/citadel\n", 1)
	got, changed := rootConfigWritePathContent(before, false)
	if !changed || got != want {
		t.Fatalf("comment backslashes blocked known-root repair:\n%s", got)
	}
}

func TestRootConfigWritePathContentPreservesOperatorLinesAndIsIdempotent(t *testing.T) {
	before := oldRootSandboxUnit()
	want := strings.Replace(before, "LimitNPROC=65535\n", "LimitNPROC=65535\nReadWritePaths=-/etc/citadel\n", 1)
	got, changed := rootConfigWritePathContent(before, false)
	if !changed || got != want {
		t.Fatalf("root repair did not append exactly one separate directive:\n%s", got)
	}
	twice, changed := rootConfigWritePathContent(got, false)
	if changed || twice != got {
		t.Fatal("root path repair is not byte-idempotent")
	}
	refreshed, decision := decideUnitRefresh(before, false, 0)
	if decision != refreshRewrite || refreshed != want {
		t.Fatalf("already-hardened root unit was not refreshed: %d\n%s", decision, refreshed)
	}
}

func TestRootConfigWritePathContentRespectsAdditivePathsAndResets(t *testing.T) {
	const original = "ReadWritePaths=/root/.citadel-cli /root/citadel-node"
	for _, test := range []struct {
		name    string
		paths   string
		changed bool
	}{
		{"missing path assignment", "", true},
		{"plain existing path", "ReadWritePaths=/root /etc/citadel /var/log", false},
		{"optional existing path", "ReadWritePaths=/root\nReadWritePaths=-/etc/citadel /var/log", false},
		{"path before reset", "ReadWritePaths=-/etc/citadel\nReadWritePaths=\nReadWritePaths=/operator/cache", true},
		{"empty final reset", "ReadWritePaths=/root -/etc/citadel\nReadWritePaths=", true},
		{"path after reset", "ReadWritePaths=-/etc/citadel\nReadWritePaths=\nReadWritePaths=/etc/citadel /operator/cache", false},
		{"lookalike is not config dir", "ReadWritePaths=/etc/citadel-backup", true},
		{"comment does not grant", "# ReadWritePaths=-/etc/citadel\nReadWritePaths=/operator/cache", true},
		{"spaces preserve operator spelling", "  ReadWritePaths = /operator/cache\n\tReadWritePaths = \nReadWritePaths = /operator/final", true},
		{"quoted effective paths hold", `ReadWritePaths="/operator/cache with space"`, false},
		{"escaped effective paths hold", `ReadWritePaths=/operator/cache\x20dir`, false},
		{"specifier paths hold", "ReadWritePaths=%h/cache", false},
		{"alternate-root paths hold", "ReadWritePaths=+/etc/citadel", false},
		{"reset clears uncertain path grammar", "ReadWritePaths=\"/operator/cache with space\"\nReadWritePaths=\nReadWritePaths=/operator/final", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := strings.Replace(oldRootSandboxUnit(), original, test.paths, 1)
			want := before
			if test.changed {
				want = strings.Replace(before, "LimitNPROC=65535\n", "LimitNPROC=65535\nReadWritePaths=-/etc/citadel\n", 1)
			}
			got, changed := rootConfigWritePathContent(before, false)
			if changed != test.changed || got != want {
				t.Fatalf("path/reset semantics changed=%v, want %v:\n%s", changed, test.changed, got)
			}
			again, changedAgain := rootConfigWritePathContent(got, false)
			if changedAgain || again != got {
				t.Fatal("path/reset result is not idempotent")
			}
		})
	}
}

func TestRootConfigWritePathContentUsesLastServiceSectionAndOnlyServiceDirectives(t *testing.T) {
	before := strings.Replace(oldRootSandboxUnit(), "[Unit]\n", "[Unit]\nReadWritePaths=-/etc/citadel\n", 1)
	before = strings.Replace(before, "[Install]\n", "[Service]\nReadWritePaths=-/etc/citadel\nReadWritePaths=\nReadWritePaths=/operator/final\nEnvironment=KEEP=unchanged\n\n[Install]\n", 1)
	want := strings.Replace(before, "Environment=KEEP=unchanged\n", "Environment=KEEP=unchanged\nReadWritePaths=-/etc/citadel\n", 1)
	got, changed := rootConfigWritePathContent(before, false)
	if !changed || got != want {
		t.Fatalf("root path did not land after the final Service reset:\n%s", got)
	}
}

func TestRootConfigWritePathContentRequiresKnownRootSystemIdentity(t *testing.T) {
	for _, test := range []struct {
		name        string
		old         string
		replacement string
		userMode    bool
		changed     bool
	}{
		{"absent User defaults to system root", "User=root\n", "", false, true},
		{"empty User resets to system root", "User=root", "User=", false, true},
		{"numeric UID zero", "User=root", "User=0", false, true},
		{"last User root", "User=root", "User=operator\nUser=root", false, true},
		{"last User nonroot", "User=root", "User=root\nUser=operator", false, false},
		{"quoted User unknown", "User=root", `User="root"`, false, false},
		{"Unicode User whitespace is unknown", "User=root", "User=operator\nUser=root\u00a0", false, false},
		{"specifier User unknown", "User=root", "User=%i", false, false},
		{"dynamic User", "User=root", "User=root\nDynamicUser=yes", false, false},
		{"unknown dynamic User", "User=root", "User=root\nDynamicUser=unknown", false, false},
		{"explicit false dynamic User", "User=root", "User=root\nDynamicUser=false", false, true},
		{"empty dynamic User is unknown", "User=root", "User=root\nDynamicUser=", false, false},
		{"empty dynamic User does not reset true", "User=root", "User=root\nDynamicUser=yes\nDynamicUser=", false, false},
		{"later explicit false dynamic User", "User=root", "User=root\nDynamicUser=yes\nDynamicUser=\nDynamicUser=false", false, true},
		{"non-strict filesystem", "ProtectSystem=strict", "ProtectSystem=true", false, false},
		{"last filesystem setting wins", "ProtectSystem=strict", "ProtectSystem=strict\nProtectSystem=false", false, false},
		{"missing filesystem setting", "ProtectSystem=strict\n", "", false, false},
		{"continued identity refuses", "User=root", "User=root\\\n  operator", false, false},
		{"continued unrelated service assignment refuses", "User=root", "User=root\nEnvironment=KEEP=one\\\n  two", false, false},
		{"user candidate is never granted", "User=root", "User=root", true, false},
		{"unknown managed marker", "Description=Citadel Worker - AceTeam Sovereign Compute", "Description=Other Worker", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := strings.Replace(oldRootSandboxUnit(), test.old, test.replacement, 1)
			got, changed := rootConfigWritePathContent(before, test.userMode)
			if changed != test.changed {
				t.Fatalf("identity changed=%v, want %v:\n%s", changed, test.changed, got)
			}
			if !changed && got != before {
				t.Fatal("refused identity changed operator content")
			}
			if changed && !strings.Contains(got, "ReadWritePaths=-/etc/citadel\n") {
				t.Fatal("positive identity control did not add the config path")
			}
		})
	}
}

func TestRootConfigPathRepairUsesExistingBackupAndFakeSystemReload(t *testing.T) {
	before := oldRootSandboxUnit()
	want := strings.Replace(before, "LimitNPROC=65535\n", "LimitNPROC=65535\nReadWritePaths=-/etc/citadel\n", 1)
	candidate := writeTempUnit(t, before, false)
	fake := &fakeSystemctl{}
	rewritten, err := rematerializeManagedUnits([]managedUnitCandidate{candidate}, 0, fake.run, nil)
	if err != nil || len(rewritten) != 1 || rewritten[0] != candidate.path {
		t.Fatalf("temp root repair = %v %v", rewritten, err)
	}
	got, err := os.ReadFile(candidate.path)
	if err != nil || string(got) != want {
		t.Fatalf("temp repaired content = %q %v", got, err)
	}
	backup, err := os.ReadFile(candidate.path + ".citadel-bak")
	if err != nil || string(backup) != before {
		t.Fatalf("backup did not preserve exact original: %q %v", backup, err)
	}
	if len(fake.calls) != 1 || strings.Join(fake.calls[0], " ") != "systemctl daemon-reload" {
		t.Fatalf("temp repair requested unexpected command: %v", fake.calls)
	}
	fakeAgain := &fakeSystemctl{}
	rewritten, err = rematerializeManagedUnits([]managedUnitCandidate{candidate}, 0, fakeAgain.run, nil)
	if err != nil || len(rewritten) != 0 || len(fakeAgain.calls) != 0 {
		t.Fatalf("second temp repair was not a no-op: %v %v %v", rewritten, err, fakeAgain.calls)
	}
	backupAgain, err := os.ReadFile(candidate.path + ".citadel-bak")
	if err != nil || string(backupAgain) != before {
		t.Fatal("idempotent repair overwrote the original backup")
	}
}

func TestRootConfigPathRepairDoesNotWriteUnknownOrUnprivilegedUnits(t *testing.T) {
	for _, test := range []struct {
		name     string
		content  string
		userMode bool
		euid     int
	}{
		{"updater is not root", oldRootSandboxUnit(), false, 1000},
		{"user manager", oldRootSandboxUnit(), true, 0},
		{"nonroot service owner", strings.Replace(oldRootSandboxUnit(), "User=root", "User=operator", 1), false, 0},
		{"dynamic service owner", strings.Replace(oldRootSandboxUnit(), "User=root", "User=root\nDynamicUser=yes", 1), false, 0},
		{"unknown service owner", strings.Replace(oldRootSandboxUnit(), "User=root", "User=%i", 1), false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := writeTempUnit(t, test.content, test.userMode)
			fake := &fakeSystemctl{}
			rewritten, err := rematerializeManagedUnits([]managedUnitCandidate{candidate}, test.euid, fake.run, nil)
			if err != nil || len(rewritten) != 0 || len(fake.calls) != 0 {
				t.Fatalf("refused temp unit was changed/reloaded: %v %v %v", rewritten, err, fake.calls)
			}
			got, err := os.ReadFile(candidate.path)
			if err != nil || string(got) != test.content {
				t.Fatalf("refused temp content changed: %q %v", got, err)
			}
			if _, err := os.Stat(candidate.path + ".citadel-bak"); !os.IsNotExist(err) {
				t.Fatalf("refused temp unit created a backup: %v", err)
			}
		})
	}
}

func TestUnknownRootRepairSyntaxDoesNotSuppressExistingRestartHardening(t *testing.T) {
	before := strings.Replace(oldRootSandboxUnit(), "User=root", "User=%i", 1)
	before = strings.Replace(before, "RestartSteps=5", "RestartSteps=99", 1)
	got, decision := decideUnitRefresh(before, false, 0)
	if decision != refreshRewrite || !strings.Contains(got, "RestartSteps=5\n") || strings.Contains(got, "ReadWritePaths=-/etc/citadel") {
		t.Fatalf("unknown identity changed permission or lost existing hardening: %d\n%s", decision, got)
	}
}
