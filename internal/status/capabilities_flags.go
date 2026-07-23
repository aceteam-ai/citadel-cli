package status

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/deskstream"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// capability_flags reports the four real node-capability booleans
// (console/desktop/files/gpu) that the AceTeam Fabric UI uses to show TRUE
// availability instead of guessing (citadel-cli#324). The backend ingests them
// inside the "capabilities" block of the heartbeat / node-status payload
// (aceteam#4223, PR #4231).
//
// Cost discipline: these run on every heartbeat (default every 30s), so the
// static signals (console + gpu) are detected once and cached, while only the
// signals that can change at runtime (desktop + files) are re-probed cheaply.

// staticCaps caches the console + gpu detection results, which do not change
// over a process's lifetime (a shell/GPU does not appear or vanish while the
// node is up). Computed once on first use via sync.Once.
var (
	staticCapsOnce sync.Once
	cachedConsole  bool
	cachedGPU      bool
	cachedH264     bool
)

// detectStaticCaps computes the console, gpu, and h264 flags exactly once.
func detectStaticCaps() {
	staticCapsOnce.Do(func() {
		cachedConsole = detectConsole()
		cachedGPU = detectGPU()
		cachedH264 = deskstream.H264Available()
	})
}

// populateCapabilityFlags fills the four capability flags on caps. The desktop
// flag is derived from vncPort (already computed by Collect, so we avoid a
// second VNC probe). console/gpu come from the cached static detection; files
// is a cheap stat of the workspace directory.
//
// Sensitive-surface gate (aceteam#6524): console/desktop/files report as
// available ONLY when the node has BOTH the underlying capability AND the
// operator has opted the matching permission in. This is what stops the Fabric
// web console from presenting a live terminal/screen/file browser for a freshly
// joined node that never opted in (the White Whale landmine) — a fresh node has
// these permissions default-DENY, so the flags read false. The operator's
// enable/disable choice remains separately visible on the heartbeat via the
// PermissionState block (permissionsToHeartbeat), so the web can still render an
// "enable + set passcode" call to action from that signal. GPU/H264 are
// hardware-only and never gated (inference must advertise regardless).
func populateCapabilityFlags(caps *NodeCapabilities, vncPort int) {
	detectStaticCaps()

	perms := config.LoadPermissions(platform.ConfigDir())

	console := cachedConsole && perms.Console
	gpu := cachedGPU
	h264 := cachedH264
	desktop := (vncPort > 0 || probeVNCPort(platform.DefaultVNCPort)) && perms.Desktop
	files := detectFiles() && perms.Files

	caps.Console = &console
	caps.Desktop = &desktop
	caps.Files = &files
	caps.GPU = &gpu
	caps.H264 = &h264
}

// detectConsole reports whether the node can offer a remote shell: a shell
// interpreter is present on PATH, or an SSH daemon is available. Citadel's
// terminal server spawns a shell, so a present shell is the meaningful signal.
func detectConsole() bool {
	var shells []string
	if runtime.GOOS == "windows" {
		shells = []string{"powershell.exe", "powershell", "cmd.exe", "cmd"}
	} else {
		shells = []string{"bash", "sh", "zsh"}
	}
	for _, sh := range shells {
		if _, err := exec.LookPath(sh); err == nil {
			return true
		}
	}
	// Fallback: an SSH daemon also satisfies "console (shell/SSH available)".
	if _, err := exec.LookPath("sshd"); err == nil {
		return true
	}
	return false
}

// detectGPU reports whether a GPU is present / inference-capable, reusing the
// platform GPU detector (the same one that feeds GPU metrics).
func detectGPU() bool {
	detector, err := platform.GetGPUDetector()
	if err != nil {
		return false
	}
	return detector.HasGPU()
}

// detectFiles reports whether node-files filesystem access is available, i.e.
// the FILE_* job handlers have a usable workspace directory. This mirrors the
// workspace resolution used by the worker (CITADEL_WORKSPACE, else
// ~/citadel-node/workspace) without importing the cmd package, and confirms the
// path is reachable.
func detectFiles() bool {
	dir := workspaceDir()
	if dir == "" {
		return false
	}
	// The directory must exist (or be creatable) and be a directory.
	info, err := os.Stat(dir)
	if err == nil {
		return info.IsDir()
	}
	// Not present yet — check the parent is writable so the workspace can be
	// created on first file job (matches resolveWorkspaceDir's MkdirAll).
	parent := filepath.Dir(dir)
	if pInfo, pErr := os.Stat(parent); pErr == nil && pInfo.IsDir() {
		return true
	}
	return false
}

// workspaceDir resolves the node-files workspace path, matching the precedence
// used by the worker's resolveWorkspaceDir (CITADEL_WORKSPACE, then
// ~/citadel-node/workspace).
func workspaceDir() string {
	if dir := os.Getenv("CITADEL_WORKSPACE"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "citadel-node", "workspace")
}

// probeVNCPort dials the local VNC port to confirm a desktop/VNC server is
// actually reachable. Used as a fallback when Collect did not already detect a
// running VNC server. Kept cheap with a short timeout since it runs per
// heartbeat.
func probeVNCPort(port int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
