// internal/network/localapi.go
// The citadel-owned local API socket that machine-wide mode publishes, and
// that other citadel processes attach to instead of starting a second mesh
// endpoint (issue #643).
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"tailscale.com/client/local"
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
	// Keep the original machine-wide TUN listener and its Windows pipe ACL.
	// Unprivileged citadel clients must still be able to attach to that API.
	ln, err := safesocket.Listen(path)
	return secureLocalAPIListener(path, ln, err)
}

// listenLocalAPIWithoutCleanup preserves the endpoint ownership decision made
// by the session caller. Its platform implementation must never unlink a
// newly-live endpoint or broaden the Windows named-pipe ACL.
func listenLocalAPIWithoutCleanup(path string) (net.Listener, error) {
	ln, err := listenSessionControlEndpoint(path)
	return secureLocalAPIListener(path, ln, err)
}

func secureLocalAPIListener(path string, ln net.Listener, err error) (net.Listener, error) {
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
	} else {
		// The Tailscale client uses safesocket's startup retry on a missing
		// pipe. A direct probe preserves the fast absence path on Windows.
		ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
		conn, err := dialLocalControlRaw(ctx, path)
		cancel()
		if err != nil {
			return false
		}
		conn.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
	defer cancel()

	// A userspace node session now publishes its control API on this SAME
	// safesocket path. A successful dial alone does not prove that it is the
	// machine-wide TUN localapi: attaching tsnet to a session controller would
	// misclassify the backend and strand the mesh. Require the real Tailscale
	// status wire response, not merely an open socket.
	lc := &local.Client{Socket: path, UseSocketOnly: true}
	_, err := lc.StatusWithoutPeers(ctx)
	return err == nil
}

// ListenSessionControl publishes the userspace session on the existing
// citadel-owned LocalAPISocketPath. It refuses ANY live listener, including
// machine-wide TUN or an unknown service, before stale-socket cleanup runs.
// Machine-wide TUN remains a separate expert mode, not a session host.
func ListenSessionControl(stateDir string) (net.Listener, error) {
	path := LocalAPISocketPath(stateDir)
	if runtime.GOOS == "windows" {
		// Named pipes are not filesystem entries. Only FILE_NOT_FOUND proves
		// absence; busy, permission-denied, and timeout are ambiguous/live and
		// must not be replaced with another pipe instance.
		ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
		defer cancel()
		conn, err := dialLocalControlRaw(ctx, path)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("local control pipe %s is already live", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("local control pipe %s may be live; refusing to replace it: %w", path, err)
		}
		return listenLocalAPIWithoutCleanup(path)
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return listenLocalAPIWithoutCleanup(path)
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("local control endpoint %s is not a socket; refusing to replace it", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		conn.Close()
		return nil, fmt.Errorf("local control endpoint %s is already live; refusing to replace it", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("local control endpoint %s may be live; refusing to replace it: %w", path, err)
	}
	// The stale inode must still be the one we probed before unlinking it.
	// The mesh holder lock excludes another Citadel TUN owner from racing in.
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return listenLocalAPIWithoutCleanup(path)
	}
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, current) {
		return nil, fmt.Errorf("local control endpoint %s changed during stale recovery; refusing to replace it", path)
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return listenLocalAPIWithoutCleanup(path)
}

// CloseSessionControl closes only the endpoint this session owned. A surviving
// Unix socket is safely handled as stale on the next start; unlinking by path
// here could remove a new listener that won a shutdown/startup race.
func CloseSessionControl(ln net.Listener, stateDir string) {
	if ln == nil {
		return
	}
	_ = ln.Close()
}

// SessionControlRequest is the client for the session verbs, over the SAME
// safesocket path. It never falls back to an unauthenticated TCP endpoint.
func SessionControlRequest(ctx context.Context, stateDir, method, route string) (*http.Response, error) {
	path := LocalAPISocketPath(stateDir)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialLocalControlRaw(ctx, path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, method, "http://citadel.local"+route, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// probeLocalControlEndpoint distinguishes the two protocols sharing the one
// citadel safesocket path. An unknown but reachable listener fails closed:
// neither a worker nor machine-wide TUN may assume it owns that endpoint.
func probeLocalControlEndpoint(stateDir string) (string, error) {
	path := LocalAPISocketPath(stateDir)
	if runtime.GOOS == "windows" {
		// An absent pipe must return promptly, even in a just-started client.
		// safesocket.ConnectContext retries missing paths for two seconds.
		ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
		defer cancel()
		conn, err := dialLocalControlRaw(ctx, path)
		if errors.Is(err, os.ErrNotExist) {
			return "none", nil
		}
		if err != nil {
			return "", fmt.Errorf("cannot probe local control pipe %s: %w", path, err)
		}
		conn.Close()
	} else {
		fi, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return "none", nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSocket == 0 {
			return "none", nil // legacy stale regular file; listener refuses it later
		}
	}
	if localAPIReachable(path) {
		return "tun", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), localAPIDialTimeout)
	defer cancel()
	if resp, err := SessionControlRequest(ctx, stateDir, http.MethodGet, "/citadel/session/v1/status"); err == nil {
		var status struct {
			Mode string `json:"mode"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && decodeErr == nil && (status.Mode == "presence" || status.Mode == "worker") {
			return "session", nil
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), localAPIDialTimeout)
	defer cancel2()
	conn, err := dialLocalControlRaw(ctx2, path)
	if err == nil {
		conn.Close()
		return "unknown", nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return "none", nil
	}
	return "", fmt.Errorf("cannot identify local control endpoint %s: %w", path, err)
}

// localAPIDialTimeout bounds the probe above. Generous for a local socket
// (which answers in microseconds) and short enough not to be felt.
const localAPIDialTimeout = 500 * time.Millisecond
