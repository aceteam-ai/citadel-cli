// internal/network/select.go
// Which Backend a citadel process gets, and the bookkeeping that keeps
// machine-wide mode from colliding with userspace mode (issue #643).
package network

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BackendMode names the transport a process ended up with.
type BackendMode string

const (
	// ModeUserspace is embedded tsnet: this process gets a mesh identity,
	// the machine does not. Unprivileged, and the default.
	ModeUserspace BackendMode = "userspace"

	// ModeTUN is a real kernel interface: the whole machine routes
	// 100.64.0.0/10. Requires root/admin, opt-in via `citadel up`.
	ModeTUN BackendMode = "tun"

	// ModeAttached is "a `citadel up` on this host already holds the mesh;
	// use it". Unprivileged — attaching needs only access to the socket.
	ModeAttached BackendMode = "attached"
)

// SelectBackend decides which transport a process should use.
//
// The attached check MUST come first. On a host running machine-wide mode, a
// `citadel work` that started its own tsnet would be a second WireGuard
// endpoint on the same node key — exactly the collision attaching exists to
// prevent. Once `citadel up` is running, every other citadel process on the
// box rides it.
//
// Otherwise: userspace, unchanged. Note this deliberately does NOT refuse
// when another userspace citadel is already connected. Several processes
// running tsnet against one state dir is the long-standing status quo
// (`citadel work` in the background while a user runs `citadel status`), and
// making that an error here would break every node for a problem this change
// did not introduce. Only the userspace/TUN collision — newly possible, and
// the one that puts a kernel interface and a userspace endpoint on the same
// identity — is prevented, in ConnectMachineWide.
func SelectBackend(stateDir string) (BackendMode, error) {
	if localAPIReachable(LocalAPISocketPath(stateDir)) {
		return ModeAttached, nil
	}
	return ModeUserspace, nil
}

// userspaceHoldersPath records the pids of processes currently running a
// userspace backend, so machine-wide mode can refuse to start alongside one.
func userspaceHoldersPath(stateDir string) string {
	return filepath.Join(stateDir, "userspace.pids")
}

// registerUserspaceHolder adds this process to the holder set and returns a
// deregister func safe to call more than once.
//
// Best-effort by design: on any I/O failure the connection still proceeds.
// This file exists to give `citadel up` a reason to refuse, not to gate the
// unprivileged path that has always worked without it.
func registerUserspaceHolder(stateDir string) func() {
	path := userspaceHoldersPath(stateDir)
	pid := os.Getpid()

	holders := append(liveHolders(path), pid)
	_ = writeHolders(path, holders)

	var done bool
	return func() {
		if done {
			return
		}
		done = true
		remaining := make([]int, 0, 4)
		for _, p := range liveHolders(path) {
			if p != pid {
				remaining = append(remaining, p)
			}
		}
		if len(remaining) == 0 {
			_ = os.Remove(path)
			return
		}
		_ = writeHolders(path, remaining)
	}
}

// liveHolders returns the recorded pids that still name running processes.
// Entries left by a crashed process are dropped rather than treated as live —
// otherwise one crash would block machine-wide mode until someone deleted the
// file by hand.
func liveHolders(path string) []int {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var live []int
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		if processAlive(pid) {
			live = append(live, pid)
		}
	}
	return live
}

func writeHolders(path string, pids []int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	parts := make([]string, 0, len(pids))
	for _, p := range pids {
		parts = append(parts, strconv.Itoa(p))
	}
	return os.WriteFile(path, []byte(strings.Join(parts, "\n")), 0o600)
}

// checkNoUserspaceHolders reports an error naming the blocking process when a
// userspace backend is live. Machine-wide mode needs exclusive use of the node
// identity: a TUN interface and a tsnet endpoint sharing one node key would
// have the coordination server handing the same node two different sets of
// endpoints.
func checkNoUserspaceHolders(stateDir string) error {
	live := liveHolders(userspaceHoldersPath(stateDir))
	if len(live) == 0 {
		return nil
	}
	return fmt.Errorf("another citadel process (pid %d) already holds this node's network identity.\n"+
		"   Machine-wide mode needs it exclusively — stop that process first, then run 'citadel up'.\n"+
		"   (Once it is up, other citadel commands on this machine attach to it automatically.)", live[0])
}
