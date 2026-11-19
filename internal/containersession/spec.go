// Package containersession runs a declared session as one container plus one
// engine-managed named volume (#1533, #1165 S1). The container's aceteam.session*
// labels are the only durable record; there is no store of our own. Every engine
// call goes through the catalog.ContainerRuntime seam (the #1041 guard), podman
// preferred and docker supported.
//
// Credentials never reach the volume: they live in a per-session sealed bundle
// beside the node config, are written to a host tmpfs run dir at wake, bind-
// mounted onto the paths the harness expects, and re-sealed (rotated tokens
// included) before that run dir is wiped at sleep.
package containersession

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

var (
	ErrSpec           = errors.New("container session specification refused")
	ErrNotContainer   = errors.New("declaration does not select the container runtime")
	ErrCredential     = errors.New("container session credential unavailable")
	ErrRunDir         = errors.New("container session run directory refused")
	ErrEngine         = errors.New("container engine operation failed")
	ErrReseal         = errors.New("container session credentials could not be re-sealed")
	ErrAlreadyRunning = errors.New("container session is already running")
)

// Fixed names. ContainerName and VolumeName are derived from the validated
// session name only, so a label can be checked against them.
const (
	ContainerPrefix = "acesess-"
	VolumeSuffix    = "-home"
	// BinaryMount is where the node's own citadel binary appears, read-only, so
	// the holder version always equals the node version at wake time.
	BinaryMount = "/usr/local/bin/citadel"
	// StopGraceSeconds bounds the holder's graceful stop before the engine kills.
	StopGraceSeconds = 30
)

// Limits is one size tier's cgroup ceiling; the same three controllers app pods
// preflight (cpu/memory/pids).
type Limits struct {
	CPUs   string
	Memory string
	Pids   int
}

// Controllers is what rt.PreflightLimitControllers must prove delegable.
var Controllers = []string{"cpu", "memory", "pids"}

var sizeLimits = map[string]Limits{
	"nano":   {CPUs: "0.5", Memory: "512m", Pids: 256},
	"small":  {CPUs: "1", Memory: "1g", Pids: 512},
	"medium": {CPUs: "2", Memory: "4g", Pids: 1024},
	"large":  {CPUs: "4", Memory: "8g", Pids: 2048},
}

// Spec is the validated, node-resolved form of one container declaration.
type Spec struct {
	Declaration   declaredsession.Declaration
	Name          string
	ContainerName string
	VolumeName    string
	Image         string
	Limits        Limits
	// HostBinary is the node's own executable, mounted read-only into the container.
	HostBinary string
	// SessionDir is the host session dir (socket, state, logs): the one place
	// `citadel attach` already dials.
	SessionDir string
	UID, GID   int
}

var safeToken = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// NewSpec validates a declaration for the container backend. It owns what the
// node may run: a bare harness binary token, a cwd of plain path characters
// (both are interpolated into the launcher script), a non-root identity.
func NewSpec(d declaredsession.Declaration, hostBinary, sessionDir string, uid, gid int) (*Spec, error) {
	if !d.IsContainer() {
		return nil, ErrNotContainer
	}
	if declaredsession.ValidateDeclaration(d) != nil || !d.Enabled {
		return nil, ErrSpec
	}
	if uid <= 0 || gid <= 0 || !path.IsAbs(hostBinary) || !path.IsAbs(sessionDir) {
		return nil, ErrSpec
	}
	if !safeToken.MatchString(d.Command[0]) || !safeToken.MatchString(d.Cwd) {
		return nil, ErrSpec
	}
	size := d.Runtime.Size
	if size == "" {
		size = "small"
	}
	limits, ok := sizeLimits[size]
	if !ok {
		return nil, ErrSpec
	}
	return &Spec{
		Declaration: d, Name: d.Name,
		ContainerName: ContainerPrefix + d.Name,
		VolumeName:    ContainerPrefix + d.Name + VolumeSuffix,
		Image:         d.Runtime.Image, Limits: limits,
		HostBinary: hostBinary, SessionDir: sessionDir, UID: uid, GID: gid,
	}, nil
}

// SessionUUID is the harness session id, derived from the name alone so it
// needs no storage and is stable across wakes and nodes. The transcript the
// harness resumes is therefore this session's own, never a sibling's.
func SessionUUID(name string) string {
	sum := sha256.Sum256([]byte("citadel-session/" + name))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// LaunchCommand renders the in-container harness argv. Resume is decided inside
// the container, from the transcript actually present on the volume, so no node
// side flag can drift from the disk: claude resumes its pinned session id when
// that transcript exists, codex resumes the latest rollout of its own volume.
// Declared extra arguments ride as positional parameters, never interpolated.
func LaunchCommand(d declaredsession.Declaration) []string {
	id := SessionUUID(d.Name)
	bin := d.Command[0]
	var script string
	switch d.Harness {
	case "codex":
		script = fmt.Sprintf(`set -e
mkdir -p '%[1]s'
cd '%[1]s'
if ls "$HOME"/.codex/sessions/*/*/*/rollout-*.jsonl >/dev/null 2>&1; then
  exec %[2]s resume --last "$@"
fi
exec %[2]s "$@"
`, d.Cwd, bin)
	default: // claude-code
		script = fmt.Sprintf(`set -e
mkdir -p '%[1]s'
cd '%[1]s'
if ls "$HOME"/.claude/projects/*/%[3]s.jsonl >/dev/null 2>&1; then
  exec %[2]s --resume %[3]s "$@"
fi
exec %[2]s --session-id %[3]s "$@"
`, d.Cwd, bin, id)
	}
	argv := []string{"/bin/sh", "-c", script, "citadel-session"}
	return append(argv, d.Command[1:]...)
}

func trimSlash(s string) string { return strings.TrimRight(s, "/") }
