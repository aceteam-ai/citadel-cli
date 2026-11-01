//go:build linux

package containerjobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestContainerHostAncestorsRefuseUntrustedOwnershipAndModes(t *testing.T) {
	for _, c := range []struct {
		uid, mode uint32
		want      bool
	}{{0, unix.S_IFDIR | 0755, true}, {0, unix.S_IFDIR | 0700, true}, {1000, unix.S_IFDIR | 0700, false}, {0, unix.S_IFDIR | 0775, false}, {0, unix.S_IFDIR | 0777, false}, {0, unix.S_IFLNK | 0700, false}, {0, unix.S_IFREG | 0600, false}} {
		if trustedHostDirectory(c.uid, c.mode) != c.want {
			t.Fatal("unsafe host ancestry accepted", c)
		}
	}
}
func TestContainerHostUnsafeParentRefusesBeforeCreatingLeaf(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0770); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(parent, "must-not-exist")
	if _, err := openContainerHostRoot(leaf); err == nil {
		t.Fatal("user-owned or world-writable ancestry accepted")
	}
	if _, err := os.Lstat(leaf); !os.IsNotExist(err) {
		t.Fatal("unsafe-parent refusal happened after mkdir")
	}
}

func nativeProvisionFixture(t *testing.T) *Native {
	t.Helper()
	root := t.TempDir()
	n := NewNativeWithPolicy(root, func() (HostPolicy, error) { return HostPolicy{Limits: fixtureHostLimits()}, nil })
	n.openHostRoot = func(path string) (*os.File, error) {
		if path != root {
			t.Fatal("fixture escaped owned root")
		}
		return os.Open(root)
	}
	return n
}

func TestContainerNativeCapacityAndComputeLimitsRefuseBeforeCommands(t *testing.T) {
	n := nativeProvisionFixture(t)
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		t.Fatal("over-budget request reached a native command")
		return commandResult{}, nil
	}
	r := baseRequest()
	r.QuotaBytes = 11 << 30
	if err := n.CreateVolume(context.Background(), r); err != ErrBounds {
		t.Fatal("quota cap missing", err)
	}
	limits := fixtureHostLimits()
	limits.MaxVolumes = 1
	n.policy = func() (HostPolicy, error) { return HostPolicy{Limits: limits}, nil }
	if os.Mkdir(filepath.Join(n.root, testVolume), 0700) != nil {
		t.Fatal("fixture")
	}
	r.VolumeRef = newVolume
	r.QuotaBytes = 16 << 20
	if err := n.CreateVolume(context.Background(), r); err != ErrBounds {
		t.Fatal("volume cap missing", err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.MemoryBytes++ }, func(r *Request) { r.PidsLimit++ }, func(r *Request) { r.CPUs = 2 }} {
		q := r
		q.MemoryBytes = 2 << 30
		q.PidsLimit = 256
		q.CPUs = 1
		change(&q)
		if err := n.CreateContainer(context.Background(), q); err != ErrBounds {
			t.Fatal("compute cap missing", err)
		}
	}
}

func TestContainerNativeNetworkRefusesBeforeLookup(t *testing.T) {
	n := nativeProvisionFixture(t)
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		t.Fatal("network request reached native lookup")
		return commandResult{}, nil
	}
	r := baseRequest()
	r.Network = "aceteam-net-1234abcd"
	if err := n.CreateContainer(context.Background(), r); err != ErrInvalid {
		t.Fatal("named bridge admitted", err)
	}
}

func TestContainerSparseReservationsCannotOvercommitHostFreeSpace(t *testing.T) {
	n := nativeProvisionFixture(t)
	n.freeHostBytes = func(*os.File) (uint64, error) { return 4 << 30, nil }
	limits := fixtureHostLimits()
	if n.hostCapacity(16<<20, limits) != nil {
		t.Fatal("empty-host positive control refused")
	}
	dir := filepath.Join(n.root, testVolume)
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	file, err := os.OpenFile(filepath.Join(dir, "volume.ext4"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if file.Truncate(3<<30) != nil || file.Close() != nil {
		t.Fatal("fixture")
	}
	if err := n.hostCapacity(16<<20, limits); err != ErrBounds {
		t.Fatal("sparse image promises were not charged against free space", err)
	}
}
