package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

type browserReadinessCheck struct {
	name, status, detail string
	fatal                bool
}

type browserReadinessProbes struct {
	goos, arch string
	chromium   func() (string, error)
	lookPath   func(string) (string, error)
	readFile   func(string) ([]byte, error)
	run        doctorCommandRunner
}

func productionBrowserReadinessProbes() browserReadinessProbes {
	return browserReadinessProbes{runtime.GOOS, runtime.GOARCH, platform.FindChromium, exec.LookPath, os.ReadFile, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}}
}

// These probes are read-only. In particular, doctor never starts a browser,
// display server, container, namespace, updater or service to test readiness.
func cobrowseReadiness(dir string, p browserReadinessProbes) []browserReadinessCheck {
	values, err := readApplianceConfig(dir)
	permissions, perr := config.LoadPermissionsStrict(dir)
	active := values["browser_appliance"] == true || (perr == nil && permissions.SetupPosture == config.SetupPostureCobrowseOnly)
	profile := browserReadinessCheck{name: "appliance profile", status: "INFO", detail: "not applied; browser checks are informational"}
	if err != nil {
		profile.status, profile.fatal = "FAIL", true
		profile.detail = err.Error()
	} else if active {
		profile.status = "OK"
		profile.detail = "applied"
	}
	checks := []browserReadinessCheck{profile, {name: "host architecture", status: "INFO", detail: p.goos + "/" + p.arch}}
	addRequired := func(name, detail string, ok bool) {
		status := "OK"
		if !ok {
			status = "INFO"
			if active {
				status = "FAIL"
			}
		}
		checks = append(checks, browserReadinessCheck{name: name, status: status, detail: detail, fatal: active && !ok})
	}
	path, chromeErr := p.chromium()
	if chromeErr != nil || path == "" {
		addRequired("Chromium/Chrome", "not found; install Chromium or Chrome", false)
	} else {
		addRequired("Chromium/Chrome", path, true)
	}
	if p.goos == "linux" {
		path, xerr := p.lookPath("Xvfb")
		if xerr != nil || path == "" {
			addRequired("Xvfb", "not found; install Xvfb for the managed display", false)
		} else {
			addRequired("Xvfb", path, true)
		}
		status, detail, blocked := browserNamespaceReadiness(p.readFile)
		checks = append(checks, browserReadinessCheck{name: "unprivileged user namespaces", status: status, detail: detail, fatal: active && blocked})
		if active && blocked {
			checks[len(checks)-1].status = "FAIL"
		}
	} else {
		checks = append(checks, browserReadinessCheck{name: "Xvfb / user namespaces", status: "INFO", detail: "Linux-only readiness checks"})
	}
	checks = append(checks, browserFontReadiness(p)...)
	guardOK := perr == nil && permissions.CobrowsePublicOnly && (!active || permissions.SetupPosture == config.SetupPostureCobrowseOnly)
	guard := browserReadinessCheck{name: "public-only egress", status: "OK", detail: "on; private-address destinations refused"}
	if !guardOK {
		guard.status, guard.detail = "INFO", "off or unreadable; private-address protection is not verified"
		if active {
			guard.status, guard.fatal = "FAIL", true
			guard.detail = "appliance posture marker or public-only guard is missing; reapply the profile"
		}
	}
	checks = append(checks, guard)
	return checks
}

func browserFontReadiness(p browserReadinessProbes) []browserReadinessCheck {
	var checks []browserReadinessCheck
	for _, font := range []struct{ name, query string }{{"fonts", ":"}, {"CJK fonts", ":lang=zh"}, {"emoji fonts", ":charset=1f600"}} {
		status, detail := "OK", "fontconfig reports coverage"
		binary, err := p.lookPath("fc-list")
		if err != nil || binary == "" {
			status, detail = "WARN", "fontconfig unavailable; coverage unknown"
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			data, err := p.run(ctx, binary, font.query, "family")
			cancel()
			if err != nil || strings.TrimSpace(string(data)) == "" {
				status, detail = "WARN", "font coverage missing or unverified; install suitable fonts"
			}
		}
		checks = append(checks, browserReadinessCheck{name: font.name, status: status, detail: detail})
	}
	return checks
}

func browserNamespaceReadiness(read func(string) ([]byte, error)) (status, detail string, blocked bool) {
	seen := false
	unknown, blockedBy := "", ""
	for _, path := range []string{"/proc/sys/user/max_user_namespaces", "/proc/sys/kernel/unprivileged_userns_clone"} {
		data, err := read(path)
		if err != nil {
			if !os.IsNotExist(err) {
				unknown = "namespace sysctl policy is unreadable; browser sandbox readiness is unknown"
			}
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			unknown = "namespace sysctl policy is malformed; browser sandbox readiness is unknown"
			continue
		}
		seen = true
		if n == 0 {
			blockedBy = path
		}
	}
	// An unknown gate must never hide an independently observed prohibition.
	// Read both gates before choosing a result, irrespective of their order.
	if blockedBy != "" {
		return "WARN", fmt.Sprintf("blocked by %s=0; Chromium's namespace sandbox cannot start", blockedBy), true
	}
	if unknown != "" {
		return "WARN", unknown, false
	}
	if data, err := read("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil {
		switch strings.TrimSpace(string(data)) {
		case "1":
			return "WARN", "AppArmor restricts unprivileged user namespaces; verify that the Chromium profile permits them", false
		case "0":
		default:
			return "WARN", "AppArmor namespace policy is unknown", false
		}
	} else if !os.IsNotExist(err) {
		return "WARN", "AppArmor namespace policy is unreadable", false
	}
	if !seen {
		return "WARN", "namespace sysctl policy could not be read; browser sandbox readiness is unknown", false
	}
	return "OK", "namespace sysctl gates are not blocked; browser sandbox execution was not tested", false
}
