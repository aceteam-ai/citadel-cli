//go:build !windows

package cmd

import (
	"os"
	"syscall"
	"testing"
)

type applianceOwnerFixture struct {
	os.FileInfo
	owner uint32
}

func (i applianceOwnerFixture) Sys() any { return &syscall.Stat_t{Uid: i.owner} }

func TestAppliancePathRequiresCurrentOwner(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if !appliancePathOwnedByCaller(applianceOwnerFixture{info, uid}) {
		t.Fatal("current owner refused")
	}
	if appliancePathOwnedByCaller(applianceOwnerFixture{info, uid + 1}) {
		t.Fatal("foreign owner accepted")
	}
}
