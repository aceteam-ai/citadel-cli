package containersession

import (
	"path"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

// mount is one bind mount.
type mount struct {
	Source, Target string
	ReadOnly       bool
}

func (m mount) arg() string {
	v := m.Source + ":" + m.Target
	if m.ReadOnly {
		v += ":ro"
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

// buildRunArgs returns the args after the engine binary for the session
// container. The image comes last among engine flags, then the holder argv, so
// nothing in the declaration can be read as an engine flag. The mounts are:
//
//   - the named volume at the fixed home (the only durable thing),
//   - the host session dir (holder socket, state, logs: where `citadel attach`
//     already dials),
//   - the rendered single-session config, read-only, from the tmpfs run dir,
//   - the node's own binary, read-only,
//   - each file credential from the tmpfs run dir onto the path the harness
//     expects (the mountpoint stub left on the volume is empty).
//
// Credential values never appear in argv: files are mounted, env values ride
// the credentials.env file that the holder's env_file reads.
func buildRunArgs(spec *Spec, in runInputs, rec Record) []string {
	args := []string{
		"-d", "--name", spec.ContainerName,
		// The reconciler, not the engine, decides what runs after a reboot, so a
		// deliberate sleep or a clean exit is never undone by an engine restart
		// pointing at a tmpfs run dir that no longer exists.
		"--restart", "no",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--cpus", spec.Limits.CPUs,
		"--memory", spec.Limits.Memory,
		"--pids-limit", strconv.Itoa(spec.Limits.Pids),
		"--user", strconv.Itoa(spec.UID) + ":" + strconv.Itoa(spec.GID),
	}
	if in.Podman {
		args = append(args, "--userns", "keep-id")
	}
	args = append(args,
		"-e", "HOME="+declaredsession.ContainerHome,
		"--tmpfs", declaredsession.ContainerNodeDir+":rw,mode=0700,uid="+strconv.Itoa(spec.UID)+",gid="+strconv.Itoa(spec.GID),
		"--tmpfs", "/tmp:rw,mode=1777",
	)
	mounts := []mount{
		{Source: spec.VolumeName, Target: declaredsession.ContainerHome},
		{Source: spec.SessionDir, Target: path.Join(declaredsession.ContainerNodeDir, "sessions", spec.Name)},
		{Source: path.Join(in.RunDir, "sessions.yaml"), Target: path.Join(declaredsession.ContainerNodeDir, "sessions.yaml"), ReadOnly: true},
		{Source: spec.HostBinary, Target: BinaryMount, ReadOnly: true},
	}
	if in.Materialized != nil {
		if in.Materialized.EnvFile != "" {
			mounts = append(mounts, mount{Source: in.Materialized.EnvFile, Target: declaredsession.ContainerCredEnvFile, ReadOnly: true})
		}
		for _, c := range spec.Declaration.Credentials {
			if c.Path != "" {
				// Writable: a harness refreshes its OAuth token in place and the
				// rotation is re-sealed at sleep.
				mounts = append(mounts, mount{Source: in.Materialized.Files[c.Name], Target: c.Path})
			}
		}
	}
	for _, m := range mounts {
		args = append(args, "-v", m.arg())
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
