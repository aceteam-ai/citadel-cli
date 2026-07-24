package nvr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Fixed, citadel-owned LOCAL paths (relative to the node owner's home) that the
// nvr compose bind-mounts. Both are LOCAL disk:
//
//   - config: Frigate's config.yml + SQLite DB. MUST stay local regardless of the
//     media storage mode — SQLite corrupts over NFS (the #597 scar). /config NEVER
//     follows the storage target.
//   - media:  recordings. For local mode this local dir IS the target. For nas
//     mode the operator/reconcile NFS-mounts the share ONTO this same path before
//     assigning the module, so the compose bind source is stable and the init
//     container's network-fs check runs against it.
//
// These mirror the `~/.citadel-cli/nvr/{config,media}` bind sources in
// services/nvr-service/compose.yml (compose expands a leading ~).
const (
	localConfigRel = ".citadel-cli/nvr/config"
	localMediaRel  = ".citadel-cli/nvr/media"
)

// ConfigDir returns the absolute local /config bind source on this node.
func ConfigDir() (string, error) { return homeJoin(localConfigRel) }

// MediaDir returns the absolute local /media bind source on this node. For nas
// mode this is the path an NFS/SMB export is mounted onto.
func MediaDir() (string, error) { return homeJoin(localMediaRel) }

func homeJoin(rel string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("nvr: resolve home dir: %w", err)
	}
	return filepath.Join(home, rel), nil
}

// ResolveMediaSource returns the HOST path bound at Frigate's /media for the given
// storage spec, plus whether the caller must first mount an external filesystem
// there (true only for nas). It does NOT itself mount anything.
//
//   - local:  Target is the media path directly (a node path).
//   - nas:    the media path is the citadel-owned MediaDir(); the caller must
//     NFS/SMB-mount Storage.Target there first and then verify it before
//     starting Frigate.
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
		mp, err := MediaDir()
		if err != nil {
			return "", false, err
		}
		return mp, true, nil
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
// export is the `host:/export` target; mountpoint is the local mountpoint (an
// absolute path — typically MediaDir()). Options force NFSv3 (Synology does not
// support v4 on the target NAS) and mount at boot. Persisting in fstab is a hard
// #597 requirement — a hand `mount` is lost on reboot and recordings then silently
// write to the local mountpoint.
func FstabEntry(export, mountpoint string) (string, error) {
	if !strings.Contains(export, ":") {
		return "", fmt.Errorf("nvr: nfs export must be host:/export, got %q", export)
	}
	if !filepath.IsAbs(mountpoint) {
		return "", fmt.Errorf("nvr: mountpoint must be absolute, got %q", mountpoint)
	}
	return fmt.Sprintf("%s %s nfs vers=3,rw,hard,noatime,_netdev 0 0", export, mountpoint), nil
}

// ---- Host-side pre-bind mount check (st_dev) ----

// MountProbe abstracts the two filesystem questions the HOST-side VerifyMediaMount
// asks, so it is unit-testable without a real NFS mount.
type MountProbe struct {
	// IsMountpoint reports whether path is an actual mountpoint on the HOST, as
	// distinct from a plain local directory (st_dev differs from the parent).
	IsMountpoint func(path string) (bool, error)
	// WritableAsUID reports whether uid can create files under path.
	WritableAsUID func(path string, uid int) (bool, error)
}

// VerifyMediaMount is the HOST-side pre-bind guard the reconcile/human runs on the
// node BEFORE the compose bind: it confirms the mountpoint really has a filesystem
// mounted (st_dev differs from parent) and is writable, catching the case where an
// NFS mount silently failed and left a plain local dir. It is distinct from the
// CONTAINER-side VerifyMediaIsNetworkFS below: inside the frigate container /media
// is ALWAYS a bind mount (st_dev always differs from parent), so an st_dev check
// there is a false pass — the shipped in-container guard must use the filesystem
// TYPE (statfs), not st_dev.
func VerifyMediaMount(path string, uid int, probe MountProbe) error {
	if probe.IsMountpoint == nil || probe.WritableAsUID == nil {
		return fmt.Errorf("nvr: MountProbe is not fully configured")
	}
	mounted, err := probe.IsMountpoint(path)
	if err != nil {
		return fmt.Errorf("nvr: checking whether %s is mounted: %w", path, err)
	}
	if !mounted {
		return fmt.Errorf("nvr: %s is NOT a mountpoint — the NFS mount failed; refusing so recordings do not silently write to the local disk", path)
	}
	writable, err := probe.WritableAsUID(path, uid)
	if err != nil {
		return fmt.Errorf("nvr: checking writability of %s: %w", path, err)
	}
	if !writable {
		return fmt.Errorf("nvr: %s is mounted but not writable — the NFS export likely needs no_root_squash (Frigate runs as root in-container)", path)
	}
	return nil
}

// DefaultMountProbe returns a host-side MountProbe backed by the real filesystem.
func DefaultMountProbe() MountProbe {
	return MountProbe{IsMountpoint: isMountpoint, WritableAsUID: writableAsUID}
}

func isMountpoint(path string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return st.Dev != parent.Dev, nil
}

func writableAsUID(dir string, uid int) (bool, error) {
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

// ---- Container-side network-filesystem check (statfs) ----
//
// Linux superblock magics for network filesystems. These are what a statfs on a
// bind-mounted /media reports as the UNDERLYING filesystem type, which is the only
// reliable way (inside the frigate container, where /media is always a bind mount)
// to tell "the host path is really an NFS/SMB share" from "the NFS mount failed and
// /media is a plain local dir".
const (
	MagicNFS  int64 = 0x6969     // NFS_SUPER_MAGIC
	MagicCIFS int64 = 0xFF534D42 // CIFS_MAGIC_NUMBER
	MagicSMB2 int64 = 0xFE534D42 // SMB2_MAGIC_NUMBER
	MagicSMB  int64 = 0x517B     // SMB_SUPER_MAGIC (older)
)

// IsNetworkFSMagic reports whether a statfs f_type value is a known network
// filesystem (NFS or SMB/CIFS).
func IsNetworkFSMagic(t int64) bool {
	switch t {
	case MagicNFS, MagicCIFS, MagicSMB2, MagicSMB:
		return true
	}
	return false
}

// NetFSProbe abstracts the two filesystem questions VerifyMediaIsNetworkFS asks,
// so the container-side guard is unit-testable without a real network mount.
type NetFSProbe struct {
	// FSType returns the statfs f_type (superblock magic) of the filesystem
	// backing path.
	FSType func(path string) (int64, error)
	// Writable reports whether uid can create files under path (Frigate runs as
	// root in-container, so a squashed export fails here).
	Writable func(path string, uid int) (bool, error)
}

// VerifyMediaIsNetworkFS is the SHIPPED, in-container guard against the #1 storage
// scar for nas mode: recordings silently landing on the small local disk because
// the NFS mount failed. Inside the frigate/init container /media is a bind mount,
// so an st_dev/mountpoint check is a false pass; this checks the filesystem TYPE
// (statfs magic) is genuinely NFS/SMB, THEN that it is root-writable
// (no_root_squash). It must fail closed so Frigate never starts writing to a
// non-network /media.
func VerifyMediaIsNetworkFS(path string, uid int, probe NetFSProbe) error {
	if probe.FSType == nil || probe.Writable == nil {
		return fmt.Errorf("nvr: NetFSProbe is not fully configured")
	}
	t, err := probe.FSType(path)
	if err != nil {
		return fmt.Errorf("nvr: statfs %s: %w", path, err)
	}
	if !IsNetworkFSMagic(t) {
		return fmt.Errorf("nvr: %s is NOT a network filesystem (statfs type 0x%x) — for storage.mode=nas the NFS/SMB export must be mounted here BEFORE start, or recordings silently write to the local disk", path, uint64(t))
	}
	writable, err := probe.Writable(path, uid)
	if err != nil {
		return fmt.Errorf("nvr: checking writability of %s: %w", path, err)
	}
	if !writable {
		return fmt.Errorf("nvr: %s is a network mount but not writable as uid %d — the export likely needs no_root_squash (Frigate runs as root in-container)", path, uid)
	}
	return nil
}

// DefaultNetFSProbe returns a NetFSProbe backed by real statfs + a probe write.
func DefaultNetFSProbe() NetFSProbe {
	return NetFSProbe{
		FSType: func(path string) (int64, error) {
			var st syscall.Statfs_t
			if err := syscall.Statfs(path, &st); err != nil {
				return 0, err
			}
			return int64(st.Type), nil
		},
		Writable: writableAsUID,
	}
}
