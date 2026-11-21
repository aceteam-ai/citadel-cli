package containersession

import (
	"path"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

// mount renders one --mount value. type=bind (not -v) is deliberate: a missing
// host source is an error instead of an engine-created, root-owned empty
// directory.
type mount struct {
	Kind     string // bind | volume
	Source   string
	Target   string
	Subpath  string // volume only
	ReadOnly bool
}

func (m mount) arg(podman bool) string {
	v := "type=" + m.Kind + ",source=" + m.Source + ",target=" + m.Target
	if m.Subpath != "" {
		key := "volume-subpath"
		if podman {
			key = "subpath"
		}
		v += "," + key + "=" + m.Subpath
	}
	if m.ReadOnly {
		v += ",readonly"
	}
	return v
}

// runInputs are the per-wake host facts the run argv depends on.
type runInputs struct {
	RunDir       string // host tmpfs run dir
	Incarnation  string
	Materialized *Materialized
	Podman       bool
}

// buildVolumeCreateArgs returns the args after the engine binary for `volume create`.
func buildVolumeCreateArgs(spec *Spec, rec Record) []string {
	args := []string{"volume", "create"}
	args = append(args, labelArgs(rec.Labels())...)
	return append(args, spec.VolumeName)
}

func baseHardening(spec *Spec, podman bool) []string {
	args := []string{
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", strconv.Itoa(spec.UID) + ":" + strconv.Itoa(spec.GID),
	}
	if podman {
		args = append(args, "--userns", "keep-id")
	}
	return args
}

// persistDirs are the in-container directories (on the volume) that must exist,
// owned by the session user, before the real mounts are placed over them.
func persistDirs(spec *Spec) []string {
	dirs := []string{spec.Config.Dir, declaredsession.ContainerWorkRoot, spec.Declaration.Cwd}
	for _, sub := range spec.Config.Persist {
		dirs = append(dirs, path.Join(spec.Config.Dir, sub))
	}
	return dirs
}

// buildInitArgs returns the args after the engine binary for the one-shot,
// network-less, capability-less container that creates those directories on the
// volume as the session user. Running it as the session user (not the engine)
// is what keeps every mountpoint parent user-owned.
func buildInitArgs(spec *Spec, podman bool) []string {
	args := []string{"run", "--rm", "--network", "none"}
	args = append(args, baseHardening(spec, podman)...)
	args = append(args,
		"--mount", mount{Kind: "volume", Source: spec.VolumeName, Target: declaredsession.ContainerHome}.arg(podman),
		"--entrypoint", "/bin/mkdir", spec.Image, "-p")
	return append(args, persistDirs(spec)...)
}

// buildRunArgs returns the args after the engine binary for the session
// container. The image comes after every engine flag, then the holder argv, so
// nothing in the declaration can be read as an engine flag. Mounts:
//
//   - the named volume at the fixed home (durable),
//   - the host tmpfs config dir over the harness config dir, and on top of it
//     each pinned transcript subpath from the volume: credentials and any
//     rename temp file land on tmpfs, transcripts on the volume,
//   - the host view dir (config, holder socket, state, logs) at /run/session,
//     bound once from a tree built on the host with the right owner and modes,
//   - the credential env directory, read-only,
//   - the node's own binary, read-only.
//
// Credential values never appear in argv.
func buildRunArgs(spec *Spec, in runInputs, rec Record) []string {
	args := []string{
		"-d", "--name", spec.ContainerName,
		// The reconciler, not the engine, decides what runs after a reboot, so a
		// deliberate sleep or a clean exit is never undone by an engine restart
		// pointing at a tmpfs run dir that no longer exists.
		"--restart", "no",
		"--cpus", spec.Limits.CPUs,
		"--memory", spec.Limits.Memory,
		"--pids-limit", strconv.Itoa(spec.Limits.Pids),
	}
	args = append(args, baseHardening(spec, in.Podman)...)
	args = append(args, "-e", "HOME="+declaredsession.ContainerHome, "--tmpfs", "/tmp:rw,mode=1777")
	mounts := []mount{
		{Kind: "volume", Source: spec.VolumeName, Target: declaredsession.ContainerHome},
		{Kind: "bind", Source: in.Materialized.ConfigDir, Target: spec.Config.Dir},
	}
	for _, sub := range spec.Config.Persist {
		rel := path.Join(path.Base(spec.Config.Dir), sub)
		mounts = append(mounts, mount{Kind: "volume", Source: spec.VolumeName, Target: path.Join(spec.Config.Dir, sub), Subpath: rel})
	}
	mounts = append(mounts,
		mount{Kind: "bind", Source: spec.ViewDir, Target: declaredsession.ContainerNodeDir},
		mount{Kind: "bind", Source: spec.HostBinary, Target: BinaryMount, ReadOnly: true},
	)
	if in.Materialized.EnvDir != "" {
		mounts = append(mounts, mount{Kind: "bind", Source: in.Materialized.EnvDir, Target: declaredsession.ContainerCredDir, ReadOnly: true})
	}
	for _, m := range mounts {
		args = append(args, "--mount", m.arg(in.Podman))
	}
	args = append(args, labelArgs(rec.Labels())...)
	args = append(args,
		"--entrypoint", BinaryMount,
		spec.Image,
		"session-host", spec.Name, "--container",
		"--node-config-dir", declaredsession.ContainerNodeDir,
		"--launch-id", in.Incarnation,
	)
	return args
}
