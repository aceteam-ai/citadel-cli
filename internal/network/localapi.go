// internal/network/localapi.go
// The citadel-owned local API socket that machine-wide mode publishes, and
// that other citadel processes attach to instead of starting a second mesh
// endpoint (issue #643).
package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"tailscale.com/safesocket"
)

func isDarwin() bool { return runtime.GOOS == "darwin" }

// LocalAPISocketPath is where a running `citadel up` publishes its backend.
// Its presence is how other citadel processes discover machine-wide mode; see
// SelectBackend.
//
// On Windows safesocket interprets a `\\.\pipe\...` path as a named pipe,
// which is the platform's equivalent and carries its own ACL.
func LocalAPISocketPath(stateDir string) string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\ProtectedPrefix\Administrators\citadel-tun`
	}
	return filepath.Join(stateDir, "tun.sock")
}

// listenLocalAPI publishes the backend socket.
//
// The socket is a full-privilege control channel: anything that can talk to it
// can reconfigure this node's mesh membership. `citadel up` runs elevated, so
// the socket must NOT be world-writable — otherwise any local user could drive
// a root-owned backend. safesocket applies 0600 on unix; the explicit chmod
// below is defence in depth against a permissive umask, and the stale-socket
// removal keeps a crashed run from blocking the next one.
func listenLocalAPI(path string) (net.Listener, error) {
	if runtime.GOOS != "windows" {
		if err := removeStaleSocket(path); err != nil {
			return nil, err
		}
	}

	ln, err := safesocket.Listen(path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			return nil, fmt.Errorf("secure %s: %w", path, err)
		}
	}
	return ln, nil
}

// removeStaleSocket clears a socket left by a crashed run. It refuses to
// remove anything that is not a socket, so a mistyped state dir can never
// make citadel delete a regular file.
func removeStaleSocket(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
	}
	return os.Remove(path)
}

func removeLocalAPISocket(path string) {
	if runtime.GOOS == "windows" {
		return // named pipes disappear with the process
	}
	_ = removeStaleSocket(path)
}

// localAPIReachable reports whether a `citadel up` is currently serving on
// the socket. A path that exists but does not accept a connection is treated
// as absent — a crashed run leaves the file behind, and attaching to a dead
// socket would be worse than starting our own backend.
//
// This runs on the hot path of every command that touches the network, so it
// must be fast in the overwhelmingly common "no machine-wide mode here" case:
//
//   - The existence check short-circuits before any dial. Without it,
//     safesocket.Connect retries for as long as its tailscaled-still-starting
//     heuristic says to, which measured ~2s per call for a path that simply
//     is not there.
//   - The dial itself is bounded, so a socket whose server is wedged (rather
//     than gone) cannot hang a citadel command indefinitely.
func localAPIReachable(path string) bool {
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
	defer cancel()

	conn, err := safesocket.ConnectContext(ctx, path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// localAPIDialTimeout bounds the probe above. Generous for a local socket
// (which answers in microseconds) and short enough not to be felt.
const localAPIDialTimeout = 500 * time.Millisecond
