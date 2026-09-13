package service

import (
	"reflect"
	"strings"
	"testing"
)

// TestRenderLaunchdPlist pins the reboot/login-survival contract on the CI host
// (the darwin-tagged TestGeneratePlist never runs on Linux). RunAtLoad+KeepAlive
// present-and-true is what makes the service start on load and relaunch on exit.
func TestRenderLaunchdPlist(t *testing.T) {
	out := renderLaunchdPlist(launchdPlistInput{
		Label:     launchdLabel,
		ExecPath:  "/opt/homebrew/bin/citadel",
		Args:      []string{"work"},
		HomeDir:   "/Users/jason",
		LogDir:    "/Users/jason/Library/Logs/citadel",
		RunAtLoad: true,
		KeepAlive: true,
	})

	mustContain := []string{
		"<string>ai.aceteam.citadel</string>",
		"<string>/opt/homebrew/bin/citadel</string>",
		"<string>work</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>CITADEL_SERVICE</key>",
		"<string>true</string>",
		"/Users/jason/Library/Logs/citadel/citadel.log",
		"/Users/jason/Library/Logs/citadel/citadel-error.log",
		`<?xml version="1.0"`,
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q:\n%s", want, out)
		}
	}

	// RunAtLoad and KeepAlive must be <true/> (reboot/login survival), not <false/>.
	if strings.Contains(out, "<key>RunAtLoad</key>\n    <false/>") {
		t.Error("RunAtLoad must be true for boot/login survival")
	}
	if strings.Contains(out, "<key>KeepAlive</key>\n    <false/>") {
		t.Error("KeepAlive must be true so the worker is relaunched on exit")
	}
}

func TestRenderLaunchdPlist_EscapesSpecialChars(t *testing.T) {
	// A home dir containing '&' would break the plist without escaping.
	out := renderLaunchdPlist(launchdPlistInput{
		Label:    launchdLabel,
		ExecPath: "/usr/local/bin/citadel",
		Args:     []string{"work", "--flag=a&b"},
		HomeDir:  "/Users/a&b",
		LogDir:   "/Users/a&b/Library/Logs/citadel",
	})
	if strings.Contains(out, "a&b</string>") {
		t.Errorf("raw '&' leaked into plist (must be escaped):\n%s", out)
	}
	if !strings.Contains(out, "a&amp;b") {
		t.Errorf("expected escaped &amp; in plist:\n%s", out)
	}
}

func TestLaunchdDomainTarget(t *testing.T) {
	if got := launchdDomainTarget(false, 0); got != "system" {
		t.Errorf("system daemon domain = %q, want system", got)
	}
	if got := launchdDomainTarget(false, 501); got != "system" {
		t.Errorf("system daemon ignores uid, got %q", got)
	}
	if got := launchdDomainTarget(true, 501); got != "gui/501" {
		t.Errorf("user agent domain = %q, want gui/501", got)
	}
}

func TestBootstrapBootoutArgs(t *testing.T) {
	if got := bootstrapArgs("gui/501", "/p.plist"); !reflect.DeepEqual(got, []string{"bootstrap", "gui/501", "/p.plist"}) {
		t.Errorf("bootstrapArgs = %v", got)
	}
	if got := bootoutArgs("system", "/p.plist"); !reflect.DeepEqual(got, []string{"bootout", "system", "/p.plist"}) {
		t.Errorf("bootoutArgs = %v", got)
	}
}

func TestParseLaunchdProgramArguments(t *testing.T) {
	plist := renderLaunchdPlist(launchdPlistInput{
		Label:    launchdLabel,
		ExecPath: "/opt/homebrew/Cellar/citadel/2.1.0/bin/citadel",
		Args:     []string{"work", "--gateway"},
		HomeDir:  "/Users/jason",
		LogDir:   "/Users/jason/Library/Logs/citadel",
	})
	got := parseLaunchdProgramArguments(plist)
	want := []string{"/opt/homebrew/Cellar/citadel/2.1.0/bin/citadel", "work", "--gateway"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLaunchdProgramArguments = %v, want %v", got, want)
	}

	if parseLaunchdProgramArguments("no program arguments here") != nil {
		t.Error("expected nil for a plist without ProgramArguments")
	}
}

func TestIsCitadelManagedPlist(t *testing.T) {
	managed := renderLaunchdPlist(launchdPlistInput{
		Label:    launchdLabel,
		ExecPath: "/opt/homebrew/bin/citadel",
		Args:     []string{"work"},
		HomeDir:  "/Users/jason",
		LogDir:   "/Users/jason/Library/Logs/citadel",
	})
	if !isCitadelManagedPlist(managed) {
		t.Error("citadel plist should be recognized as managed")
	}

	// Right label but not a `work` invocation -> not adopted.
	notWork := renderLaunchdPlist(launchdPlistInput{
		Label:    launchdLabel,
		ExecPath: "/opt/homebrew/bin/citadel",
		Args:     []string{"status"},
		HomeDir:  "/Users/jason",
		LogDir:   "/Users/jason/Library/Logs/citadel",
	})
	if isCitadelManagedPlist(notWork) {
		t.Error("a non-`work` citadel plist should not be treated as the managed node service")
	}

	// A foreign plist at our path must never be adopted.
	foreign := renderLaunchdPlist(launchdPlistInput{
		Label:    "com.example.other",
		ExecPath: "/usr/bin/other",
		Args:     []string{"work"},
		HomeDir:  "/Users/jason",
		LogDir:   "/tmp",
	})
	if isCitadelManagedPlist(foreign) {
		t.Error("a non-citadel plist must not be treated as managed")
	}
}

// TestReplaceFirstProgramArgument pins the launchd rematerialize heal: a
// Cellar-baked ExecPath is repointed to the stable Homebrew symlink, args are
// preserved, and an already-stable plist is left untouched (no churn).
func TestReplaceFirstProgramArgument(t *testing.T) {
	orig := renderLaunchdPlist(launchdPlistInput{
		Label:     launchdLabel,
		ExecPath:  "/opt/homebrew/Cellar/citadel/2.1.0/bin/citadel",
		Args:      []string{"work", "--gateway"},
		HomeDir:   "/Users/jason",
		LogDir:    "/Users/jason/Library/Logs/citadel",
		RunAtLoad: true,
		KeepAlive: true,
	})

	updated, changed := replaceFirstProgramArgument(orig, "/opt/homebrew/bin/citadel")
	if !changed {
		t.Fatal("expected a change when repointing the Cellar ExecPath")
	}
	args := parseLaunchdProgramArguments(updated)
	want := []string{"/opt/homebrew/bin/citadel", "work", "--gateway"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("after heal args = %v, want %v", args, want)
	}
	// Args and the RunAtLoad/KeepAlive keys must survive.
	if !strings.Contains(updated, "<key>RunAtLoad</key>") || !strings.Contains(updated, "--gateway") {
		t.Error("heal must preserve the rest of the plist")
	}

	// Idempotent: re-running against the already-stable ExecPath changes nothing.
	again, changed2 := replaceFirstProgramArgument(updated, "/opt/homebrew/bin/citadel")
	if changed2 || again != updated {
		t.Error("replacing an already-correct ExecPath must be a no-op")
	}
}
