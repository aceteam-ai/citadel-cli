//go:build unix

package network

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStateObservationAliasesAndSpecialFiles(t *testing.T) {
	// Fail before entering this FIFO-capable fixture if a future flag change
	// could make its direct native open wait indefinitely for a writer.
	source, err := os.ReadFile("state_observation_unix.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK") {
		t.Fatal("nonblocking native open prerequisite changed")
	}
	base := t.TempDir()
	target := filepath.Join(base, "target")
	alias := filepath.Join(base, "alias")
	if err := os.WriteFile(target, []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if data, exists, err := readStateObservationFile(alias, 16); err != nil || !exists || string(data) != "config" {
		t.Fatalf("trusted regular alias = %q, %t, %v", data, exists, err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dangling, filepath.Join(dangling, "network")} {
		if _, exists, err := readStateObservationFile(path, 16); exists || err != errStateObservation {
			t.Fatal("dangling alias must not count as missing configuration")
		}
		if present, err := ObserveStatePresence(path); present || err != errStateObservation {
			t.Fatal("dangling alias must not count as missing state")
		}
	}
	fifo := filepath.Join(base, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readStateObservationFile(fifo, 16); exists || err != errStateObservation {
		t.Fatal("FIFO must refuse before reading")
	}
	// Exercise the actual nonblocking open seam too: replacing a previously
	// inspected regular file with a FIFO must not block in open before fstat.
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openStateObservation(fifo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readStateObservationFD(f, fifo, before, 16); err != errStateObservation {
		t.Error("special descriptor was not refused before read")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(base, "state")
	stateAlias := filepath.Join(base, "state-alias")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stateDir, stateAlias); err != nil {
		t.Fatal(err)
	}
	if present, err := ObserveStatePresence(stateAlias); present || err != nil {
		t.Fatalf("valid empty directory alias = %t, %v", present, err)
	}
	if present, err := ObserveStatePresence(filepath.Join(stateAlias, "missing")); present || err != nil {
		t.Fatalf("missing below valid directory alias = %t, %v", present, err)
	}
}

func TestStateObservationRejectsSubstitutedDescriptorAndPath(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "config")
	replacement := filepath.Join(base, "replacement")
	for _, name := range []string{path, replacement} {
		if err := os.WriteFile(name, []byte("same-size"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := openStateObservation(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readStateObservationFD(wrong, path, before, 32); err != errStateObservation {
		t.Error("substituted descriptor accepted")
	}
	if err := wrong.Close(); err != nil {
		t.Fatal(err)
	}
	held, err := openStateObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readStateObservationFD(held, path, before, 32); err != errStateObservation {
		t.Error("substituted pathname accepted")
	}
}

func TestSessionControlEndpointAbsentUnixRefusal(t *testing.T) {
	if !SessionControlEndpointAbsent(fmt.Errorf("dial: %w", unix.ECONNREFUSED)) {
		t.Fatal("Unix connection refusal must classify endpoint absent")
	}
	for _, err := range []error{unix.EACCES, unix.EPERM, unix.ETIMEDOUT, unix.EINTR, unix.ENOTDIR} {
		if SessionControlEndpointAbsent(fmt.Errorf("dial: %w", err)) {
			t.Fatalf("non-absence errno classified absent: %v", err)
		}
	}
}
