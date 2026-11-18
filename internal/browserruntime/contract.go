// Package browserruntime owns the per-session run contract of the
// browser-runtime image (aceteam-ai/citadel-cli#1759, epic #1757): the labels a
// session container carries, the loopback-only dynamic port publishes, the
// optional profile mount, and the non-root user. The C2 container runner builds
// its `docker|podman run` argv through RunSpec.Args so there is one definition.
//
// The image is the meeting-service image under its new name. Nothing here
// starts a container; it only builds argv and parses what the engine reports.
package browserruntime

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// Image is the canonical image name; LegacyImage is the alias published
	// with the same digest for one release so existing references keep working.
	Image       = "ghcr.io/aceteam-ai/browser-runtime"
	LegacyImage = "ghcr.io/aceteam-ai/meeting-service"

	// Container-side ports fixed by the image (Dockerfile ENV). The host side
	// is always a dynamic loopback port, read back with ParsePublishedPort.
	ControlContainerPort = 8102 // meetingd control API
	CDPContainerPort     = 9223 // Chromium DevTools via the in-image socat forward

	// Container paths. ProfileMountPath is the optional profile mount;
	// WorkspaceMountPath is the optional meetings/ mount for meeting sessions.
	ProfileMountPath   = "/profile"
	WorkspaceMountPath = "/workspace/meetings"

	// Container labels. The session label is what sweep and re-adopt filter on.
	LabelRuntime = "aceteam.browser-runtime"         // "1" on every session container
	LabelSession = "aceteam.browser-runtime.session" // session id
	LabelPurpose = "aceteam.browser-runtime.purpose" // meeting|huddle|cobrowse|bot_sign_in|bot_probe

	// Image labels (set at build time) and the in-image file with the same major.
	ImageLabelContract       = "aceteam.browser-runtime.contract"
	ImageLabelChromiumMajor  = "aceteam.browser-runtime.chromium-major"
	ContractVersion          = "1"
	ChromiumMajorFileInImage = "/etc/browser-runtime/chromium-major"
)

var (
	sessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	imageRe     = regexp.MustCompile(`^ghcr\.io/aceteam-ai/(browser-runtime|meeting-service)(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}$`)
	purposes    = map[string]bool{"meeting": true, "huddle": true, "cobrowse": true, "bot_sign_in": true, "bot_probe": true}
)

// RunSpec describes one session container.
type RunSpec struct {
	// Image is a digest-pinned ref (name[:tag]@sha256:<digest>) under Image or
	// LegacyImage; required.
	Image string
	// Network is required and must equal NetworkName(SessionID): a network that
	// exists only for this session. The image binds meetingd and the CDP
	// forwarder on 0.0.0.0 inside the container and neither is authenticated, so
	// "127.0.0.1::PORT" only restricts the HOST side; on a shared network any
	// other container (other sessions, app pods) could reach <ip>:9223 and drive
	// CDP, cookies included. C2's duties: create the network fresh (an "already
	// exists" error is a failure, never reuse), attach only this session's
	// container to it, and delete it at teardown or sweep. Residual: processes on
	// the host can reach the container IP on the bridge subnet, the same trust
	// boundary as the 127.0.0.1 publish.
	Network   string
	SessionID string
	Purpose   string
	UID, GID  int // docker --user; must be non-zero
	// ProfileDir is an optional existing, clean, absolute host dir mounted rw at
	// /profile; the caller owns it as UID:GID. Precondition: it is mounted into at
	// most one container at a time, because the entrypoint deletes Chromium's
	// Singleton* files. Without it /profile is a tmpfs (throwaway profile).
	ProfileDir string
	// WorkspaceMeetingsDir is an optional existing, clean, absolute host dir
	// mounted rw at /workspace/meetings.
	WorkspaceMeetingsDir string
	ShmSize              string // default 1g (chromium crashes on docker's 64m)
	CPUs                 string // default 1.5
	Memory               string // default 1536m
}

// Args returns the argv after the engine name, ending with the image. It does
// not include --name so the caller picks the container name.
//
// Conservative choices: no userns remap and no chown (the process runs as
// UID:GID from the start), all capabilities dropped, no-new-privileges,
// read-only root with tmpfs for /run, /tmp and /home/bot owned by UID:GID, and
// both ports published to 127.0.0.1 with a dynamic host port.
func (s RunSpec) Args() ([]string, error) {
	if err := validateImage(s.Image); err != nil {
		return nil, err
	}
	if s.Network != NetworkName(s.SessionID) {
		return nil, fmt.Errorf("browserruntime: network %q must be NetworkName(session) %q", s.Network, NetworkName(s.SessionID))
	}
	if !sessionIDRe.MatchString(s.SessionID) {
		return nil, fmt.Errorf("browserruntime: invalid session id %q", s.SessionID)
	}
	if !purposes[s.Purpose] {
		return nil, fmt.Errorf("browserruntime: invalid purpose %q", s.Purpose)
	}
	if s.UID <= 0 || s.GID <= 0 {
		return nil, errors.New("browserruntime: UID and GID must be non-zero (chromium refuses root)")
	}
	for _, d := range []string{s.ProfileDir, s.WorkspaceMeetingsDir} {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) || filepath.Clean(d) != d || d == "/" || strings.ContainsAny(d, ",\n\r\"") {
			return nil, fmt.Errorf("browserruntime: mount dir %q must be absolute, clean, not / and free of ',' and newlines", d)
		}
	}
	def := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	owner := fmt.Sprintf("uid=%d,gid=%d", s.UID, s.GID)
	args := []string{
		"run", "-d",
		"--user", fmt.Sprintf("%d:%d", s.UID, s.GID),
		"--network", s.Network,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/run:rw,size=64m,mode=0700," + owner,
		"--tmpfs", "/tmp:rw,size=512m,mode=1777," + owner,
		"--tmpfs", "/home/bot:rw,size=256m,mode=0700," + owner,
		"--shm-size", def(s.ShmSize, "1g"),
		"--cpus", def(s.CPUs, "1.5"),
		"--memory", def(s.Memory, "1536m"),
		"--pids-limit", "1024",
		"-p", fmt.Sprintf("127.0.0.1::%d", ControlContainerPort),
		"-p", fmt.Sprintf("127.0.0.1::%d", CDPContainerPort),
		"--label", LabelRuntime + "=1",
		"--label", LabelSession + "=" + s.SessionID,
		"--label", LabelPurpose + "=" + s.Purpose,
	}
	// --mount (unlike -v) errors on a missing source instead of creating a
	// root-owned dir.
	if s.ProfileDir != "" {
		args = append(args, "--mount", "type=bind,src="+s.ProfileDir+",dst="+ProfileMountPath)
	} else {
		// The root is read-only; Chromium needs a writable user-data-dir.
		args = append(args, "--tmpfs", ProfileMountPath+":rw,size=512m,mode=0700,"+owner)
	}
	if s.WorkspaceMeetingsDir != "" {
		args = append(args, "--mount", "type=bind,src="+s.WorkspaceMeetingsDir+",dst="+WorkspaceMountPath)
	}
	return append(args, s.Image), nil
}

func validateImage(img string) error {
	if !imageRe.MatchString(img) {
		return fmt.Errorf("browserruntime: image %q must be ghcr.io/aceteam-ai/{browser-runtime,meeting-service}[:tag]@sha256:<digest>", img)
	}
	return nil
}

// NetworkName is the per-session network name C2 must create for a session.
func NetworkName(sessionID string) string { return "aceteam-br-" + sessionID }

// SessionFilterArgs returns the `ps` filters that find one session's container
// (runtime label AND session label), or every session container when sessionID
// is empty. Labels are not authentication: C2's re-adopt must also verify the
// recorded container ID or CDP identity before trusting a match.
func SessionFilterArgs(sessionID string) ([]string, error) {
	args := []string{"--filter", "label=" + LabelRuntime + "=1"}
	if sessionID != "" {
		if !sessionIDRe.MatchString(sessionID) {
			return nil, fmt.Errorf("browserruntime: invalid session id %q", sessionID)
		}
		args = append(args, "--filter", "label="+LabelSession+"="+sessionID)
	}
	return args, nil
}

var publishedPortRe = regexp.MustCompile(`^127\.0\.0\.1:(\d{1,5})$`)

// An IPv6 loopback line is tolerated (and ignored) in dual-binding output; the
// contract only publishes 127.0.0.1.
var ignoredLoopbackRe = regexp.MustCompile(`^\[::1\]:\d{1,5}$`)

// ParsePublishedPort reads the output of `<engine> port <container> <cport>/tcp`
// and returns the host port. It accepts only loopback bindings and fails
// closed on anything else, so a mis-published (all-interfaces) port is never
// used. When the engine lists several bindings every one must be loopback.
func ParsePublishedPort(out string) (int, error) {
	port := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if ignoredLoopbackRe.MatchString(line) {
			continue
		}
		m := publishedPortRe.FindStringSubmatch(line)
		if m == nil {
			return 0, fmt.Errorf("browserruntime: published port %q is not loopback-only", line)
		}
		p, _ := strconv.Atoi(m[1])
		if p < 1 || p > 65535 {
			return 0, fmt.Errorf("browserruntime: port %d out of range", p)
		}
		if port == 0 {
			port = p
		}
	}
	if port == 0 {
		return 0, errors.New("browserruntime: no published port reported")
	}
	return port, nil
}

// ParseChromiumMajor validates the chromium-major image label (or file) value.
func ParseChromiumMajor(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("browserruntime: invalid chromium major %q", v)
	}
	return n, nil
}
