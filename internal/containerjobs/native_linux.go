//go:build linux

package containerjobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Native struct {
	root        string
	policy      func() (HostPolicy, error)
	command     commandFunc
	execControl execControl
}

func NewNative(root string, runtime func() string) *Native {
	return NewNativeWithPolicy(root, func() (HostPolicy, error) {
		p := HostPolicy{}
		if runtime != nil {
			p.Runtime = runtime()
		}
		return p, nil
	})
}

func NewNativeWithPolicy(root string, policy func() (HostPolicy, error)) *Native {
	return &Native{root: root, policy: policy, command: runCommand, execControl: newDockerExecClient()}
}

// Quota provisioning needs a privileged Linux worker and installed host tools.
// Looking up executables is passive; readiness never executes a probe.
func NativeSupported() bool {
	if os.Geteuid() != 0 {
		return false
	}
	for _, bin := range []string{"docker", "mkfs.ext4", "mount", "umount", "chown"} {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	return true
}
func (n *Native) do(ctx context.Context, bin string, args ...string) ([]byte, error) {
	result, err := n.command(ctx, bin, args...)
	if err != nil || result.code != 0 || result.truncated || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	return result.bytes, nil
}
func (n *Native) Lock(ctx context.Context) (func(), error) {
	if !filepath.IsAbs(n.root) || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if err := os.MkdirAll(n.root, 0700); err != nil {
		return nil, ErrUnavailable
	}
	st, err := os.Lstat(n.root)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, ErrUnavailable
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrUnavailable
	}
	root, err := os.OpenRoot(n.root)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer root.Close()
	f, err := root.OpenFile("host.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrUnavailable
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, ErrUnavailable
	}
	for {
		if ctx.Err() != nil {
			f.Close()
			return nil, ErrUnavailable
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, ErrUnavailable
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ErrUnavailable
		case <-timer.C:
		}
	}
}
func (n *Native) volumePaths(ref string) (dir, image, mount string, err error) {
	if !volumePattern.MatchString(ref) || !filepath.IsAbs(n.root) {
		return "", "", "", ErrInvalid
	}
	dir = filepath.Join(n.root, ref)
	return dir, filepath.Join(dir, "volume.ext4"), filepath.Join(dir, "root"), nil
}
func (n *Native) Container(ctx context.Context, ref string) (ContainerState, error) {
	if !containerPattern.MatchString(ref) && !hashPattern.MatchString(ref) {
		return ContainerState{}, ErrInvalid
	}
	b, err := n.do(ctx, "docker", "container", "inspect", ref)
	if err != nil {
		return ContainerState{}, err
	}
	var rows []struct {
		ID      string
		ExecIDs json.RawMessage
		Config  struct{ Labels map[string]string }
		State   struct{ Running *bool }
		Mounts  []struct{ Name, Destination, Type string }
	}
	if json.Unmarshal(b, &rows) != nil || len(rows) != 1 {
		return ContainerState{}, ErrUnavailable
	}
	row := rows[0]
	var execs []string
	if !hashPattern.MatchString(row.ID) || row.State.Running == nil || len(row.ExecIDs) == 0 || json.Unmarshal(row.ExecIDs, &execs) != nil || len(execs) > 1024 {
		return ContainerState{}, ErrUnavailable
	}
	for _, id := range execs {
		if !hashPattern.MatchString(id) {
			return ContainerState{}, ErrUnavailable
		}
	}
	volume := ""
	for _, m := range row.Mounts {
		if m.Destination == "/workspace" && m.Type == "volume" {
			if volume != "" {
				return ContainerState{}, ErrUnavailable
			}
			volume = m.Name
		}
	}
	if !volumePattern.MatchString(volume) || row.Config.Labels["aceteam.volume_ref"] != volume {
		return ContainerState{}, ErrOwnership
	}
	return ContainerState{ID: row.ID, ExecIDs: execs, OrgID: row.Config.Labels["aceteam.org_id"], VolumeRef: volume, Running: *row.State.Running}, nil
}
func (n *Native) Volume(ctx context.Context, ref string) (VolumeState, error) {
	_, _, mount, err := n.volumePaths(ref)
	if err != nil {
		return VolumeState{}, err
	}
	names, listErr := n.do(ctx, "docker", "volume", "ls", "--filter", "name=^"+ref+"$", "--format", "{{.Name}}")
	if listErr != nil {
		return VolumeState{}, ErrUnavailable
	}
	if strings.TrimSpace(string(names)) == "" {
		return VolumeState{}, ErrNotFound
	}
	b, err := n.do(ctx, "docker", "volume", "inspect", ref)
	if err != nil {
		return VolumeState{}, err
	}
	var rows []struct {
		Driver          string
		Labels, Options map[string]string
	}
	if json.Unmarshal(b, &rows) != nil || len(rows) != 1 || rows[0].Driver != "local" || rows[0].Options["type"] != "none" || rows[0].Options["o"] != "bind" || rows[0].Options["device"] != mount {
		return VolumeState{}, ErrOwnership
	}
	row := rows[0]
	quota, err := strconv.ParseInt(row.Labels["aceteam.quota_bytes"], 10, 64)
	if err != nil || !validQuota(quota) || !canonicalUUID(row.Labels["aceteam.org_id"]) {
		return VolumeState{}, ErrOwnership
	}
	return VolumeState{OrgID: row.Labels["aceteam.org_id"], QuotaBytes: quota}, nil
}
func (n *Native) CreateVolume(ctx context.Context, r Request) error {
	dir, image, mount, err := n.volumePaths(r.VolumeRef)
	if err != nil || !canonicalUUID(r.OrgID) || !validQuota(r.QuotaBytes) {
		return ErrInvalid
	}
	b, err := n.do(ctx, "docker", "volume", "ls", "--filter", "name=^"+r.VolumeRef+"$", "--format", "{{.Name}}")
	if err != nil || strings.TrimSpace(string(b)) != "" {
		return ErrUnavailable
	}
	if os.Mkdir(dir, 0700) != nil {
		return ErrUnavailable
	}
	f, err := os.OpenFile(image, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrUnavailable
	}
	err = f.Truncate(r.QuotaBytes)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	if _, err = n.do(ctx, "mkfs.ext4", "-q", "-F", "-m", "0", "-E", "lazy_itable_init=0,lazy_journal_init=0", image); err != nil {
		return err
	}
	if os.Mkdir(mount, 0700) != nil {
		return ErrUnavailable
	}
	if _, err = n.do(ctx, "mount", "-t", "ext4", "-o", "loop,nodev,nosuid", image, mount); err != nil {
		return err
	}
	// Only the freshly created volume root changes ownership. Never recursively
	// rewrite imported or preserved data, or chown the private host image parent.
	owner := strconv.Itoa(WorkspaceUID) + ":" + strconv.Itoa(WorkspaceGID)
	if _, err = n.do(ctx, "chown", "--no-dereference", "--", owner, mount); err != nil {
		return err
	}
	_, err = n.do(ctx, "docker", "volume", "create", "--driver", "local", "--opt", "type=none", "--opt", "o=bind", "--opt", "device="+mount,
		"--label", "aceteam.org_id="+r.OrgID, "--label", "aceteam.quota_bytes="+strconv.FormatInt(r.QuotaBytes, 10), r.VolumeRef)
	if err != nil {
		return err
	}
	state, err := n.Volume(ctx, r.VolumeRef)
	if err != nil || state.OrgID != r.OrgID || state.QuotaBytes != r.QuotaBytes {
		return ErrOwnership
	}
	return nil
}
func (n *Native) CreateContainer(ctx context.Context, r Request) error {
	policy := HostPolicy{}
	if n.policy != nil {
		var err error
		policy, err = n.policy()
		if err != nil {
			return ErrUnavailable
		}
	}
	if err := policy.validate(); err != nil {
		return err
	}
	volume, err := n.Volume(ctx, r.VolumeRef)
	if err != nil || volume.OrgID != r.OrgID || volume.QuotaBytes != r.QuotaBytes {
		return ErrOwnership
	}
	attached, err := n.do(ctx, "docker", "ps", "-aq", "--filter", "volume="+r.VolumeRef)
	if err != nil || strings.TrimSpace(string(attached)) != "" {
		return ErrUnavailable
	}
	root, err := n.OpenVolume(ctx, r.VolumeRef)
	if err != nil {
		return err
	}
	root.Close()
	b, err := n.do(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	var info struct {
		Driver       string
		DriverStatus [][]string
	}
	if json.Unmarshal(b, &info) != nil {
		return ErrUnavailable
	}
	layerLimit := info.Driver == "btrfs" || info.Driver == "zfs" || info.Driver == "devicemapper"
	for _, pair := range info.DriverStatus {
		if len(pair) == 2 && pair[0] == "Backing Filesystem" && pair[1] == "xfs" && info.Driver == "overlay2" {
			layerLimit = true
		}
	}
	if r.Network != "" {
		if r.Network != "aceteam-net-"+strings.TrimPrefix(r.ContainerRef, "sandbox-") {
			return ErrInvalid
		}
		networks, err := n.do(ctx, "docker", "network", "ls", "--filter", "name=^"+r.Network+"$", "--format", "{{.Name}}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(networks)) == "" {
			if _, err = n.do(ctx, "docker", "network", "create", "--driver", "bridge", "--opt", "com.docker.network.bridge.enable_icc=false", "--label", "aceteam.org_id="+r.OrgID, "--label", "aceteam.container_ref="+r.ContainerRef, r.Network); err != nil {
				return err
			}
		}
		b, err := n.do(ctx, "docker", "network", "inspect", r.Network)
		if err != nil {
			return err
		}
		var rows []struct {
			Driver          string
			Labels, Options map[string]string
		}
		if json.Unmarshal(b, &rows) != nil || len(rows) != 1 || rows[0].Driver != "bridge" || rows[0].Labels["aceteam.org_id"] != r.OrgID || rows[0].Labels["aceteam.container_ref"] != r.ContainerRef || rows[0].Options["com.docker.network.bridge.enable_icc"] != "false" {
			return ErrOwnership
		}
	}
	args, err := createArguments(r, policy, layerLimit)
	if err != nil {
		return err
	}
	if _, err = n.do(ctx, "docker", args...); err != nil {
		return err
	}
	state, err := n.Container(ctx, r.ContainerRef)
	if err != nil || state.OrgID != r.OrgID || state.VolumeRef != r.VolumeRef {
		return ErrOwnership
	}
	return nil
}
func (n *Native) Start(ctx context.Context, ref string) error {
	state, err := n.Container(ctx, ref)
	if err != nil {
		return err
	}
	// Never start against an unmounted host directory after a reboot. The
	// operator's host setup must restore its exact loop mounts first.
	root, err := n.OpenVolume(ctx, state.VolumeRef)
	if err != nil {
		return err
	}
	root.Close()
	_, err = n.do(ctx, "docker", "start", ref)
	if err != nil {
		return err
	}
	current, err := n.Container(ctx, ref)
	if err != nil || !current.Running || current.OrgID != state.OrgID || current.VolumeRef != state.VolumeRef {
		return ErrUnavailable
	}
	return nil
}
func (n *Native) Stop(ctx context.Context, ref string) error {
	before, err := n.Container(ctx, ref)
	if err != nil {
		return err
	}
	if _, err = n.do(ctx, "docker", "stop", "--signal", "TERM", "--time", "10", before.ID); err != nil {
		return err
	}
	after, err := n.Container(ctx, before.ID)
	if err != nil || after.Running || after.ID != before.ID || after.OrgID != before.OrgID || after.VolumeRef != before.VolumeRef {
		return ErrUnavailable
	}
	return nil
}

// EXEC cancellation terminates the whole container in v1. Killing only the
// Docker client does not stop its remote process. The volume is never removed.
func (n *Native) Exec(ctx context.Context, ref string, argv []string) (ExecResult, error) {
	before, err := n.Container(ctx, ref)
	if err != nil || !canonicalUUID(before.OrgID) || !hashPattern.MatchString(before.ID) || n.execControl == nil {
		return ExecResult{}, ErrUnavailable
	}
	// A surviving remote exec from another worker incarnation carries no new
	// execution authority. Refuse unless its actual exit can be inspected.
	for _, previous := range before.ExecIDs {
		state, err := n.execControl.Inspect(ctx, previous)
		if err != nil || state.Running || state.ContainerID != before.ID {
			return ExecResult{}, ErrUnavailable
		}
	}
	id, err := n.execControl.Create(ctx, ref, argv)
	if err != nil {
		return ExecResult{}, ErrUnavailable
	}
	initial, err := n.execControl.Inspect(ctx, id)
	if err != nil || initial.Running || initial.ContainerID != before.ID {
		return ExecResult{}, ErrUnavailable
	}
	output := &boundedOutput{}
	stream, attachErr := n.execControl.Attach(ctx, id)
	startObserved := attachErr == nil
	if attachErr == nil {
		// Closing the local attach is only I/O cancellation, never proof of exit.
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = stream.Close()
			case <-done:
			}
		}()
		attachErr = readExecOutput(ctx, stream, output)
		close(done)
		_ = stream.Close()
	}
	result := ExecResult{Output: output.bytes, Truncated: output.truncated}
	for ctx.Err() == nil {
		state, err := n.execControl.Inspect(ctx, id)
		if err == nil && state.ContainerID == before.ID && state.Running {
			startObserved = true
		}
		if err == nil && state.ContainerID == before.ID && !state.Running && startObserved {
			if ctx.Err() != nil {
				break
			}
			if attachErr != nil {
				result.Truncated = true
				result.Message = "exec finished; some output was unavailable"
			}
			result.ExitCode = state.ExitCode
			return result, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	same := func() (ContainerState, error) {
		current, e := n.Container(cleanup, before.ID)
		if e != nil || current.ID != before.ID || current.OrgID != before.OrgID || current.VolumeRef != before.VolumeRef {
			return current, ErrOwnership
		}
		return current, nil
	}
	current, err := same()
	if err != nil {
		return result, ErrUnavailable
	}
	if current.Running {
		_ = n.Stop(cleanup, before.ID) // Docker sends TERM, waits ten seconds, then KILL.
		current, err = same()
		if err != nil {
			return result, ErrUnavailable
		}
		if current.Running {
			if _, err = n.do(cleanup, "docker", "kill", "--signal", "KILL", before.ID); err != nil {
				return result, ErrUnavailable
			}
			current, err = same()
			if err != nil || current.Running {
				return result, ErrUnavailable
			}
		}
	}
	if cleanup.Err() != nil {
		return result, ErrUnavailable
	}
	result.TimedOut = true
	result.ExitCode = 124
	return result, nil
}

func (n *Native) OpenVolume(ctx context.Context, ref string) (*os.Root, error) {
	volume, err := n.Volume(ctx, ref)
	if err != nil {
		return nil, err
	}
	_, image, mount, err := n.volumePaths(ref)
	if err != nil {
		return nil, err
	}
	imageInfo, err := os.Lstat(image)
	if err != nil || !imageInfo.Mode().IsRegular() || imageInfo.Size() != volume.QuotaBytes {
		return nil, ErrUnavailable
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, ErrUnavailable
	}
	f, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, ErrUnavailable
	}
	defer f.Close()
	var stat unix.Stat_t
	var statfs unix.Statfs_t
	if unix.Fstat(int(f.Fd()), &stat) != nil || unix.Fstatfs(int(f.Fd()), &statfs) != nil || statfs.Type != unix.EXT4_SUPER_MAGIC {
		root.Close()
		return nil, ErrUnavailable
	}
	// The filesystem must be the loop device backed by this exact private image,
	// not an unmounted host directory or a different volume at the mount point.
	dev := strconv.FormatUint(uint64(unix.Major(uint64(stat.Dev))), 10) + ":" + strconv.FormatUint(uint64(unix.Minor(uint64(stat.Dev))), 10)
	backing, err := os.ReadFile("/sys/dev/block/" + dev + "/loop/backing_file")
	if err != nil {
		root.Close()
		return nil, ErrUnavailable
	}
	target := strings.TrimSuffix(string(backing), "\n")
	if !filepath.IsAbs(target) {
		target = "/" + target
	}
	backingInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(imageInfo, backingInfo) {
		root.Close()
		return nil, ErrUnavailable
	}
	return root, nil
}
func (n *Native) Usage(ctx context.Context, ref string) (int64, int64, error) {
	v, err := n.Volume(ctx, ref)
	if err != nil {
		return 0, 0, err
	}
	root, err := n.OpenVolume(ctx, ref)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return 0, 0, ErrUnavailable
	}
	defer f.Close()
	var stat unix.Statfs_t
	if unix.Fstatfs(int(f.Fd()), &stat) != nil || stat.Bfree > stat.Blocks || stat.Bsize <= 0 || stat.Blocks > uint64(v.QuotaBytes)/uint64(stat.Bsize) {
		return 0, 0, ErrUnavailable
	}
	return int64(stat.Blocks-stat.Bfree) * stat.Bsize, v.QuotaBytes, nil
}
func (n *Native) DestroyContainer(ctx context.Context, ref string) error {
	state, err := n.Container(ctx, ref)
	if err != nil {
		return err
	}
	network := "aceteam-net-" + strings.TrimPrefix(ref, "sandbox-")
	found, err := n.do(ctx, "docker", "network", "ls", "--filter", "name=^"+network+"$", "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	present := strings.TrimSpace(string(found)) != ""
	if present {
		raw, err := n.do(ctx, "docker", "network", "inspect", network)
		if err != nil {
			return err
		}
		var rows []struct{ Labels map[string]string }
		if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || rows[0].Labels["aceteam.org_id"] != state.OrgID || rows[0].Labels["aceteam.container_ref"] != ref {
			return ErrOwnership
		}
	}
	if _, err = n.do(ctx, "docker", "rm", ref); err != nil {
		return err
	}
	if present {
		_, err = n.do(ctx, "docker", "network", "rm", network)
	}
	return err
}
func (n *Native) DestroyVolume(ctx context.Context, ref string) error {
	_, image, mount, err := n.volumePaths(ref)
	if err != nil {
		return err
	}
	b, err := n.do(ctx, "docker", "ps", "-aq", "--filter", "volume="+ref)
	if err != nil || strings.TrimSpace(string(b)) != "" {
		return ErrUnavailable
	}
	root, err := n.OpenVolume(ctx, ref)
	if err != nil {
		return err
	}
	root.Close()
	if _, err = n.do(ctx, "docker", "volume", "rm", ref); err != nil {
		return err
	}
	if _, err = n.do(ctx, "umount", mount); err != nil {
		return err
	}
	// Keep the private directory as a tombstone. Never reuse a partially removed
	// volume ref, and never recursively delete a path that might still be mounted.
	if os.Remove(image) != nil || os.Remove(mount) != nil {
		return ErrUnavailable
	}
	return nil
}
