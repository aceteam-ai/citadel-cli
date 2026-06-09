package cmd

import "testing"

func TestAppListCommand(t *testing.T) {
	// Use a temp home so state file is empty (no Docker calls needed).
	t.Setenv("HOME", t.TempDir())

	// runAppList writes to os.Stdout via tabwriter; we just verify it
	// runs without error. The tabwriter output is visible in -v mode.
	err := runAppList(appListCmd, nil)
	if err != nil {
		t.Fatalf("runAppList() error = %v", err)
	}
}

func TestAppStopNotInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := runAppStop(appStopCmd, []string{"code-server"})
	if err == nil {
		t.Fatal("runAppStop() expected error for not-installed app")
	}
	if err.Error() != "app code-server is not installed" {
		t.Errorf("runAppStop() error = %q, want %q", err.Error(), "app code-server is not installed")
	}
}

func TestAppStartNotInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := runAppStart(appStartCmd, []string{"jupyter"})
	if err == nil {
		t.Fatal("runAppStart() expected error for not-installed app")
	}
	if err.Error() != "app jupyter is not installed. Run 'citadel app install jupyter' first" {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestAppUninstallNotInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := runAppUninstall(appUninstallCmd, []string{"filebrowser"})
	if err == nil {
		t.Fatal("runAppUninstall() expected error for not-installed app")
	}
	if err.Error() != "app filebrowser is not installed" {
		t.Errorf("unexpected error: %q", err.Error())
	}
}

func TestAppInstallUnknownApp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := runAppInstall(appInstallCmd, []string{"nonexistent-app"})
	if err == nil {
		t.Fatal("runAppInstall() expected error for unknown app")
	}
}

func TestAppStatusEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Should print "no apps installed" message and not error.
	err := runAppStatus(appStatusCmd, nil)
	if err != nil {
		t.Fatalf("runAppStatus() error = %v", err)
	}
}
