package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func desktopTestSource(t *testing.T, root, app, contents string) string {
	t.Helper()
	path := filepath.Join(root, app+".app", "Contents", "MacOS", "citadel-aarch64-apple-darwin")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func desktopTestPlist(t *testing.T, home, executable string, args ...string) string {
	t.Helper()
	path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := renderLaunchdPlist(launchdPlistInput{
		Label: launchdLabel, ExecPath: executable, Args: args,
		HomeDir: home, LogDir: filepath.Join(home, "Library", "Logs", "citadel"),
		RunAtLoad: true, KeepAlive: true, PathEnv: launchdServicePATH,
	})
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readDesktopTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDesktopReconcileUpgradeMoveAndAlreadyCurrent(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	oldSource := desktopTestSource(t, root, "Old Citadel", "old helper")
	oldHelper, err := installDesktopHelper(oldSource, home)
	if err != nil {
		t.Fatal(err)
	}
	plist := desktopTestPlist(t, home, oldHelper, "--no-auto-update", "work")
	// These are the node's existing credentials and network identity. Neither
	// reconciliation nor app movement may alter them or trigger enrollment.
	identity := filepath.Join(home, ".citadel", "identity.key")
	if err := os.MkdirAll(filepath.Dir(identity), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("same enrolled node"), 0o600); err != nil {
		t.Fatal(err)
	}
	newSource := desktopTestSource(t, root, "Moved Citadel", "new helper")
	if err := os.Remove(oldSource); err != nil {
		t.Fatal(err)
	}
	var restarts int
	status, err := ReconcileDesktopHelper(newSource, home, func(path string) error {
		if path != plist {
			t.Fatalf("restarted wrong plist: %s", path)
		}
		restarts++
		return nil
	})
	if err != nil || status != DesktopRestarted || restarts != 1 {
		t.Fatalf("upgrade status=%q restarts=%d err=%v", status, restarts, err)
	}
	args := parseLaunchdProgramArguments(readDesktopTestFile(t, plist))
	if len(args) != 3 || args[0] == oldHelper || !reflect.DeepEqual(args[1:], []string{"--no-auto-update", "work"}) {
		t.Fatalf("new launchd arguments: %v", args)
	}
	if got := readDesktopTestFile(t, args[0]); got != "new helper" {
		t.Fatalf("new helper contents: %q", got)
	}
	if got := readDesktopTestFile(t, oldHelper); got != "old helper" {
		t.Fatalf("old helper was overwritten: %q", got)
	}
	if got := readDesktopTestFile(t, identity); got != "same enrolled node" {
		t.Fatalf("enrollment identity changed: %q", got)
	}
	firstPlist := readDesktopTestFile(t, plist)
	status, err = ReconcileDesktopHelper(newSource, home, func(string) error {
		restarts++
		return nil
	})
	if err != nil || status != DesktopUnchanged || restarts != 1 || readDesktopTestFile(t, plist) != firstPlist {
		t.Fatalf("already-current service churned: status=%q restarts=%d err=%v", status, restarts, err)
	}
}

func TestDesktopReconcileOldBundledPathAndMissingCurrentHelper(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	oldBundle := desktopTestSource(t, root, "Translocated Citadel", "old bundle")
	newBundle := desktopTestSource(t, root, "Applications Citadel", "new bundle")
	plist := desktopTestPlist(t, home, oldBundle, "work")
	if err := os.Remove(oldBundle); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	status, err := ReconcileDesktopHelper(newBundle, home, func(string) error { restarts++; return nil })
	if err != nil || status != DesktopRestarted || restarts != 1 {
		t.Fatalf("old bundle migration: status=%q restarts=%d err=%v", status, restarts, err)
	}
	args := parseLaunchdProgramArguments(readDesktopTestFile(t, plist))
	if !desktopManagedExec(args[0], home) || !reflect.DeepEqual(args[1:], []string{"--no-auto-update", "work"}) {
		t.Fatalf("migration arguments: %v", args)
	}
	if err := os.Remove(args[0]); err != nil {
		t.Fatal(err)
	}
	status, err = ReconcileDesktopHelper(newBundle, home, func(string) error { restarts++; return nil })
	if err != nil || status != DesktopRestarted || restarts != 2 || readDesktopTestFile(t, args[0]) != "new bundle" {
		t.Fatalf("missing current helper not repaired: status=%q restarts=%d err=%v", status, restarts, err)
	}
}

func TestDesktopReconcileCorruptHelperRefusesMutation(t *testing.T) {
	home := t.TempDir()
	source := desktopTestSource(t, t.TempDir(), "Citadel", "signed helper bytes")
	current, err := installDesktopHelper(source, home)
	if err != nil {
		t.Fatal(err)
	}
	plist := desktopTestPlist(t, home, current, "--no-auto-update", "work")
	before := readDesktopTestFile(t, plist)
	if err := os.WriteFile(current, []byte("corrupt bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	status, err := ReconcileDesktopHelper(source, home, func(string) error {
		t.Fatal("corrupt helper must not trigger restart")
		return nil
	})
	if status != DesktopStageFailed || err == nil || readDesktopTestFile(t, plist) != before {
		t.Fatalf("corrupt helper must fail closed: status=%q err=%v", status, err)
	}
}

func TestDesktopReconcileRefusesUnmanagedService(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		args       []string
		wrongLabel bool
	}{
		{name: "manual executable", path: "/usr/local/bin/citadel", args: []string{"work"}},
		{name: "other command", args: []string{"status"}},
		{name: "other launchd label", args: []string{"work"}, wrongLabel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			source := desktopTestSource(t, t.TempDir(), "Citadel", "new helper")
			path := test.path
			if path == "" {
				path = source
			}
			plist := desktopTestPlist(t, home, path, test.args...)
			if test.wrongLabel {
				content := strings.Replace(readDesktopTestFile(t, plist), "<string>"+launchdLabel+"</string>", "<string>com.example.other</string>", 1)
				if err := os.WriteFile(plist, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := readDesktopTestFile(t, plist)
			status, err := ReconcileDesktopHelper(source, home, func(string) error {
				t.Fatal("unmanaged service must not be restarted")
				return nil
			})
			if err != nil || status != DesktopUnmanaged || readDesktopTestFile(t, plist) != before {
				t.Fatalf("unmanaged service was adopted: status=%q err=%v", status, err)
			}
		})
	}
}

func TestDesktopReconcileRestartFailureDoesNotLoop(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	old := desktopTestSource(t, root, "Old Citadel", "old")
	newer := desktopTestSource(t, root, "New Citadel", "new")
	plist := desktopTestPlist(t, home, old, "work")
	restarts := 0
	status, err := ReconcileDesktopHelper(newer, home, func(string) error {
		restarts++
		return errors.New("launchd unavailable")
	})
	if status != DesktopRestartFailed || err == nil || restarts != 1 {
		t.Fatalf("restart failure: status=%q restarts=%d err=%v", status, restarts, err)
	}
	args := parseLaunchdProgramArguments(readDesktopTestFile(t, plist))
	if len(args) != 3 || args[0] == old || readDesktopTestFile(t, args[0]) != "new" {
		t.Fatalf("failed restart did not retain staged update: %v", args)
	}
	status, err = ReconcileDesktopHelper(newer, home, func(string) error {
		restarts++
		return nil
	})
	if err != nil || status != DesktopUnchanged || restarts != 1 {
		t.Fatalf("restart failure caused a loop: status=%q restarts=%d err=%v", status, restarts, err)
	}
}

func TestDesktopReconcileStagesWithoutEnrolling(t *testing.T) {
	home := t.TempDir()
	source := desktopTestSource(t, t.TempDir(), "Citadel", "new helper")
	status, err := ReconcileDesktopHelper(source, home, func(string) error {
		t.Fatal("must not restart before a service exists")
		return nil
	})
	if err != nil || status != DesktopStaged {
		t.Fatalf("new app stage: status=%q err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".citadel")); !os.IsNotExist(err) {
		t.Fatal("staging unexpectedly created enrollment data")
	}
}
