//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSavePermissionsKeepsTheFileOwner(t *testing.T) {
	dir := t.TempDir()
	if err := SavePermissions(dir, DefaultPermissions()); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dir, permissionsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePermissions(dir, DefaultPermissions()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dir, permissionsFile))
	if err != nil {
		t.Fatal(err)
	}
	b, a := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if b.Uid != a.Uid || b.Gid != a.Gid {
		t.Fatalf("owner changed across save: %d:%d -> %d:%d", b.Uid, b.Gid, a.Uid, a.Gid)
	}
}

func TestSavePermissionsRefusesWhenOwnerCannotBeKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chown to any owner")
	}
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	// A file owned by root (uid 0) cannot be given to it by a non-root writer.
	if _, err := os.Stat("/"); err != nil {
		t.Skip("no root-owned path to compare against")
	}
	if err := keepOwner(tmp, "/"); err == nil {
		t.Fatal("keepOwner must refuse when the existing owner cannot be kept")
	}
}

func TestOwnerDecision(t *testing.T) {
	denied := func() error { return errors.New("operation not permitted") }
	ok := func() error { return nil }
	for _, tc := range []struct {
		name                     string
		tmpUID, tmpGID, uid, gid uint32
		chown                    func() error
		wantErr                  bool
	}{
		{"same owner and group", 1000, 1000, 1000, 1000, denied, false},
		{"owner differs, chown works", 0, 0, 1000, 1000, ok, false},
		{"owner differs, chown denied", 1000, 1000, 0, 0, denied, true},
		{"only group differs, chown denied", 1000, 1000, 1000, 27, denied, false},
		{"only group differs, chown works", 1000, 1000, 1000, 27, ok, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ownerDecision(tc.tmpUID, tc.tmpGID, tc.uid, tc.gid, tc.chown)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ownerDecision err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
