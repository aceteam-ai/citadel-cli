package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
)

type fakeManagedTmuxInstaller struct {
	present       bool
	ensureCalled  bool
	installCalled bool
	ensureOK      bool
	ensureErr     error
}

func (f *fakeManagedTmuxInstaller) AlreadyInstalled() bool { return f.present }
func (f *fakeManagedTmuxInstaller) DestPath() string       { return "/managed/tmux" }
func (f *fakeManagedTmuxInstaller) Ensure() (bool, error) {
	f.ensureCalled = true
	return f.ensureOK, f.ensureErr
}
func (f *fakeManagedTmuxInstaller) Install() error {
	f.installCalled = true
	return nil
}

func withTmuxInstallSeams(t *testing.T, installer managedTmuxInstaller, available bool) {
	t.Helper()
	originalNew := newManagedTmuxInstaller
	originalAvailable := managedTmuxArtifactAvailable
	originalForce := tmuxInstallForce
	newManagedTmuxInstaller = func() managedTmuxInstaller { return installer }
	managedTmuxArtifactAvailable = func() bool { return available }
	tmuxInstallForce = false
	t.Cleanup(func() {
		newManagedTmuxInstaller = originalNew
		managedTmuxArtifactAvailable = originalAvailable
		tmuxInstallForce = originalForce
	})
}

func TestRunTmuxInstallDoesNotAcceptUnsupportedExistingBinary(t *testing.T) {
	unsupported := fmt.Errorf("%w: found 2.5, require >= %s", tmux.ErrTmuxVersionUnsupported, tmux.MinimumVersion)
	installer := &fakeManagedTmuxInstaller{present: true, ensureErr: unsupported}
	withTmuxInstallSeams(t, installer, false)

	err := runTmuxInstall(nil, nil)
	if !errors.Is(err, tmux.ErrTmuxVersionUnsupported) {
		t.Fatalf("runTmuxInstall() error = %v, want ErrTmuxVersionUnsupported", err)
	}
	if !installer.ensureCalled || installer.installCalled {
		t.Fatalf("calls: ensure=%v install=%v; old binary must be validated, not accepted or force-installed", installer.ensureCalled, installer.installCalled)
	}
}

func TestRunTmuxInstallForceRequiresArtifactAndSkipsAlreadyInstalledShortcut(t *testing.T) {
	installer := &fakeManagedTmuxInstaller{present: true, ensureOK: true}
	withTmuxInstallSeams(t, installer, false)
	tmuxInstallForce = true

	err := runTmuxInstall(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no managed tmux artifact") {
		t.Fatalf("runTmuxInstall(--force) error = %v, want unavailable-artifact error", err)
	}
	if installer.ensureCalled || installer.installCalled {
		t.Fatalf("calls: ensure=%v install=%v; gated force must not claim or attempt replacement", installer.ensureCalled, installer.installCalled)
	}
}
