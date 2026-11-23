package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func fakeBrowserReadinessProbes() browserReadinessProbes {
	return browserReadinessProbes{goos: "linux", arch: "amd64", chromium: func() (string, error) { return "fixture-chrome", nil }, lookPath: func(name string) (string, error) { return name, nil }, readFile: func(path string) ([]byte, error) {
		if strings.Contains(path, "apparmor") {
			return []byte("0"), nil
		}
		return []byte("1024"), nil
	}, run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte("fixture font"), nil
	}}
}

func applianceDoctorDir(t *testing.T, active bool) string {
	t.Helper()
	dir := t.TempDir()
	if active {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("browser_appliance: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeSetupPosture(dir, t.TempDir(), "appliance"); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestApplianceDoctorReadinessRequiredOnlyWhenApplied(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, missing := range []string{"none", "browser", "Xvfb", "namespace", "egress"} {
			t.Run(strings.Join([]string{map[bool]string{true: "applied", false: "ordinary"}[active], missing}, "/"), func(t *testing.T) {
				dir := applianceDoctorDir(t, active)
				p := fakeBrowserReadinessProbes()
				switch missing {
				case "browser":
					p.chromium = func() (string, error) { return "", errors.New("missing") }
				case "Xvfb":
					p.lookPath = func(name string) (string, error) {
						if name == "Xvfb" {
							return "", errors.New("missing")
						}
						return name, nil
					}
				case "namespace":
					p.readFile = func(string) ([]byte, error) { return []byte("0"), nil }
				case "egress":
					if err := config.SavePermissions(dir, config.DefaultPermissions()); err != nil {
						t.Fatal(err)
					}
				}
				checks := cobrowseReadiness(dir, p)
				r := doctorReport{dockerHealth: platform.DockerHealth{OK: true}, doctor: healthyDoctorPayload(), browserReadiness: checks}
				wantOK := !active || missing == "none"
				if r.ok() != wantOK {
					t.Fatalf("ok=%v want %v: %+v", r.ok(), wantOK, checks)
				}
				if !wantOK && r.problem() == "unknown problem" {
					t.Fatal("missing actionable refusal")
				}
				var out bytes.Buffer
				renderDoctorReport(&out, r)
				for _, text := range []string{"CO-BROWSE READINESS", "Chromium/Chrome", "Xvfb", "host architecture", "linux/amd64", "public-only egress", "CJK fonts", "emoji fonts", "unprivileged user namespaces"} {
					if !strings.Contains(out.String(), text) {
						t.Fatalf("missing %q", text)
					}
				}
			})
		}
	}
}

func TestApplianceDoctorRefusesUnknownConfigAndRetainsPostureEvidence(t *testing.T) {
	for _, kind := range []string{"malformed", "unreadable", "missing-with-posture"} {
		t.Run(kind, func(t *testing.T) {
			dir := applianceDoctorDir(t, true)
			path := filepath.Join(dir, "config.yaml")
			switch kind {
			case "malformed":
				if err := os.WriteFile(path, []byte("[damaged:"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("platform does not enforce POSIX read permissions")
				}
			case "missing-with-posture":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			p := fakeBrowserReadinessProbes()
			p.chromium = func() (string, error) { return "", os.ErrNotExist }
			checks := cobrowseReadiness(dir, p)
			r := doctorReport{dockerHealth: platform.DockerHealth{OK: true}, doctor: healthyDoctorPayload(), browserReadiness: checks}
			if r.ok() {
				t.Fatal("unknown configuration or known posture cannot hide missing browser")
			}
			if kind != "missing-with-posture" && (!checks[0].fatal || checks[0].status != "FAIL") {
				t.Fatalf("config read failure must be fatal: %+v", checks[0])
			}
		})
	}
}

func TestApplianceDoctorFontsAndUnknownNamespacesWarn(t *testing.T) {
	dir := applianceDoctorDir(t, true)
	p := fakeBrowserReadinessProbes()
	p.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("missing fonts") }
	p.readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	checks := cobrowseReadiness(dir, p)
	warnings := 0
	for _, check := range checks {
		if check.fatal {
			t.Fatalf("warnings must not be fatal: %+v", check)
		}
		if check.status == "WARN" {
			warnings++
		}
	}
	if warnings != 4 {
		t.Fatalf("need fonts, CJK, emoji and unknown namespace warnings: %+v", checks)
	}
	p = fakeBrowserReadinessProbes()
	p.readFile = func(path string) ([]byte, error) {
		if strings.Contains(path, "apparmor") {
			return []byte("1"), nil
		}
		return []byte("1024"), nil
	}
	checks = cobrowseReadiness(dir, p)
	found := false
	for _, check := range checks {
		if strings.Contains(check.detail, "AppArmor") {
			found = true
			if check.status != "WARN" || check.fatal {
				t.Fatal(check)
			}
		}
	}
	if !found {
		t.Fatal("AppArmor restriction must be explained")
	}
}

func TestApplianceDoctorNonLinuxAndProbeIsolation(t *testing.T) {
	p := fakeBrowserReadinessProbes()
	p.goos, p.arch = "darwin", "arm64"
	p.lookPath = func(name string) (string, error) {
		if name != "fc-list" {
			t.Fatal("Linux probe on another platform")
		}
		return "", os.ErrNotExist
	}
	p.readFile = func(string) ([]byte, error) { t.Fatal("Linux sysctl read on another platform"); return nil, nil }
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("Linux font probe on another platform")
		return nil, nil
	}
	checks := cobrowseReadiness(applianceDoctorDir(t, true), p)
	for _, check := range checks {
		if check.fatal {
			t.Fatal(check)
		}
	}
}

func TestApplianceDoctorNamespaceUnknownIsNotClaimedReady(t *testing.T) {
	for _, failure := range []string{"malformed", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			status, _, blocked := browserNamespaceReadiness(func(string) ([]byte, error) {
				if failure == "unreadable" {
					return nil, os.ErrPermission
				}
				return []byte("bad"), nil
			})
			if status != "WARN" || blocked {
				t.Fatalf("must report unknown, not ready or known blocked: %s %v", status, blocked)
			}
		})
	}
}

func TestApplianceDoctorFontProbesAreBoundedAndWarnWhenMissing(t *testing.T) {
	p := fakeBrowserReadinessProbes()
	count := 0
	p.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("font probe must have a deadline")
		}
		if name != "fc-list" || len(args) != 2 || args[1] != "family" {
			t.Fatal("unexpected command")
		}
		count++
		return []byte(""), nil
	}
	for _, check := range browserFontReadiness(p) {
		if check.status != "WARN" || check.fatal {
			t.Fatal(check)
		}
	}
	if count != 3 {
		t.Fatal(count)
	}
	p.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("ran a missing tool")
		return nil, nil
	}
	for _, check := range browserFontReadiness(p) {
		if check.status != "WARN" || check.fatal {
			t.Fatal(check)
		}
	}
}
