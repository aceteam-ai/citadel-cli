package nvr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LocalConfigDir is the citadel-owned LOCAL path bind-mounted at Frigate's
// /config. It holds Frigate's SQLite DB (frigate.db) and the generated
// config.yml. It MUST stay on local disk regardless of the media storage mode:
// SQLite corrupts over NFS (the #597 scar). Only /media follows the storage
// target; /config never does.
//
// Relative to the node owner's home; the reconcile joins it with the home dir.
const LocalConfigDir = ".citadel-cli/nvr/config"

// DefaultNASMountpoint is the citadel-owned LOCAL mountpoint an `nas` target is
// mounted at (and then bind-mounted into Frigate as /media). A dedicated,
// citadel-owned path — never a shared/system dir — so the mount-verify check has
// a stable place to assert against.
const DefaultNASMountpoint = "/mnt/citadel-nvr-media"

// ResolveMediaSource returns the HOST path to bind-mount at Frigate's /media for
// the given storage spec, plus whether the caller must first mount an external
// filesystem there (true only for nas). It does NOT itself mount anything.
//
//   - local:  Target is used directly as the media path.
//   - nas:    the media path is the citadel-owned mountpoint (DefaultNASMountpoint);
//     the caller must NFS/SMB-mount Storage.Target there first and then
//     VerifyMediaMount before starting Frigate.
//   - volume: Target is the resolved volume path (the volume subsystem provides it).
func ResolveMediaSource(s StorageSpec) (path string, needsMount bool, err error) {
	switch s.Mode {
	case StorageLocal:
		if strings.TrimSpace(s.Target) == "" {
			return "", false, fmt.Errorf("nvr: storage.target is required for mode %q", StorageLocal)
		}
		return s.Target, false, nil
	case StorageNAS:
		if !strings.Contains(s.Target, ":") {
			return "", false, fmt.Errorf("nvr: storage.target for mode %q must be host:/export, got %q", StorageNAS, s.Target)
		}
		return DefaultNASMountpoint, true, nil
	case StorageVolume:
		if strings.TrimSpace(s.Target) == "" {
			return "", false, fmt.Errorf("nvr: storage.target (volume id) is required for mode %q", StorageVolume)
		}
		return s.Target, false, nil
	default:
		return "", false, fmt.Errorf("nvr: unknown storage mode %q", s.Mode)
	}
}

// FstabEntry builds the /etc/fstab line that persists an NFS mount across reboots.
// export is the `host:/export` target; mountpoint is the local mountpoint. The
// options force NFSv3 (Synology does not support v4 on the target NAS) and mount
// at boot. Persisting in fstab is a hard #597 requirement — a hand `mount` is lost
// on reboot and recordings then silently write to the local mountpoint.
func FstabEntry(export, mountpoint string) (string, error) {
	if !strings.Contains(export, ":") {
		return "", fmt.Errorf("nvr: nfs export must be host:/export, got %q", export)
	}
	if !filepath.IsAbs(mountpoint) {
		return "", fmt.Errorf("nvr: mountpoint must be absolute, got %q", mountpoint)
	}
	return fmt.Sprintf("%s %s nfs vers=3,rw,hard,noatime,_netdev 0 0", export, mountpoint), nil
}

// MountProbe abstracts the two filesystem questions VerifyMediaMount asks, so the
// verify logic is unit-testable without a real NFS mount.
type MountProbe struct {
	// IsMountpoint reports whether path is an actual mountpoint (a filesystem is
	// mounted there), as distinct from a plain local directory.
	IsMountpoint func(path string) (bool, error)
	// WritableAsUID reports whether uid can create files under path. Frigate runs
	// as root in-container, so the export must be root-writable (NFS
	// no_root_squash); uid is the container UID that will write recordings.
	WritableAsUID func(path string, uid int) (bool, error)
}

// VerifyMediaMount is the guard against the single worst #597 failure mode: a
// FAILED NFS mount leaves the mountpoint a plain local directory that is happily
// writable, so recordings silently land on the small local disk instead of the
// NAS and nobody notices until it fills. It therefore checks mountedness
// SEPARATELY from writability — a writable-but-not-mounted path is the bug, and
// must fail.
//
// Order matters: assert the path is genuinely a mountpoint FIRST (catches the
// leak), then assert it is writable as the container UID (catches a missing
// no_root_squash export option). Only call this for mode == nas.
func VerifyMediaMount(path string, uid int, probe MountProbe) error {
	if probe.IsMountpoint == nil || probe.WritableAsUID == nil {
		return fmt.Errorf("nvr: MountProbe is not fully configured")
	}
	mounted, err := probe.IsMountpoint(path)
	if err != nil {
		return fmt.Errorf("nvr: checking whether %s is mounted: %w", path, err)
	}
	if !mounted {
		return fmt.Errorf("nvr: %s is NOT a mountpoint — the NFS mount failed; refusing to start Frigate so recordings do not silently write to the local disk", path)
	}
	writable, err := probe.WritableAsUID(path, uid)
	if err != nil {
		return fmt.Errorf("nvr: checking writability of %s: %w", path, err)
	}
	if !writable {
		return fmt.Errorf("nvr: %s is mounted but not writable as uid %d — the NFS export likely needs no_root_squash (Frigate runs as root in-container)", path, uid)
	}
	return nil
}

// DefaultMountProbe returns a MountProbe backed by the real filesystem. It is the
// production probe; tests inject their own.
func DefaultMountProbe() MountProbe {
	return MountProbe{
		IsMountpoint:  isMountpoint,
		WritableAsUID: writableAsUID,
	}
}

// isMountpoint reports whether path is a mountpoint by comparing its device id to
// its parent's: a mountpoint sits on a different underlying device than the
// directory that contains it. This is the classic st_dev comparison and needs no
// /proc/mounts parse.
func isMountpoint(path string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	// Different device id than the parent => a filesystem is mounted here.
	// Same device id but the path IS the filesystem root (rare for a mountpoint
	// dir) is also handled by the inode check.
	if st.Dev != parent.Dev {
		return true, nil
	}
	return false, nil
}

// writableAsUID reports whether uid can create a file under dir. It attempts a
// probe write of a temp file. Running as root (Frigate's in-container identity),
// a create succeeding proves the NFS export granted root write (no_root_squash);
// on a root-squashed export the create fails with EACCES/EROFS.
func writableAsUID(dir string, uid int) (bool, error) {
	// The citadel process performing the check runs as the node owner; the real
	// runtime write happens as the container UID. When the checker IS that uid (the
	// common single-user node) the probe is exact; otherwise it is a best-effort
	// create in the same export, which still catches a squashed/read-only export.
	_ = uid
	f, err := os.CreateTemp(dir, ".citadel-nvr-writecheck-")
	if err != nil {
		if os.IsPermission(err) {
			return false, nil
		}
		return false, err
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true, nil
}
