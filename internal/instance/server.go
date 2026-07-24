// Package instance provides single-instance detection and session attach
// for the Citadel TUI. The first TUI instance creates a Unix domain socket
// at ~/.citadel-cli/citadel.sock; subsequent invocations detect it and
// attach as raw terminal clients (similar to tmux attach).
//
//go:build !windows

package instance

import (
	"encoding/binary"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/console"
)

const socketName = "citadel.sock"

// Server listens on a Unix domain socket and relays PTY sessions
// to attached clients.
type Server struct {
	ln       net.Listener
	sockPath string
	mu       sync.Mutex
	clients  []net.Conn
	closed   bool
}

// Listen creates a Unix socket server at configDir/citadel.sock.
// Returns nil, nil if another instance already holds the socket.
func Listen(configDir string) (*Server, error) {
	sockPath := filepath.Join(configDir, socketName)

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}

	// Try to connect first — if we can, another instance is alive
	if conn, err := net.Dial("unix", sockPath); err == nil {
		conn.Close()
		return nil, nil
	}

	// Stale socket: remove and try again
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}

	// Restrict socket permissions
	os.Chmod(sockPath, 0600)

	s := &Server{
		ln:       ln,
		sockPath: sockPath,
	}
	go s.acceptLoop()
	return s, nil
}

// SocketPath returns the path for client connections.
func SocketPath(configDir string) string {
	return filepath.Join(configDir, socketName)
}

// IsRunning checks if another instance is listening on the socket.
func IsRunning(configDir string) bool {
	sockPath := filepath.Join(configDir, socketName)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// PID returns the PID stored in configDir/citadel.pid, or 0 if absent.
func PID(configDir string) int {
	data, err := os.ReadFile(filepath.Join(configDir, "citadel.pid"))
	if err != nil {
		return 0
	}
	var pid int
	for _, b := range data {
		if b >= '0' && b <= '9' {
			pid = pid*10 + int(b-'0')
		}
	}
	return pid
}

// WritePID writes the current PID to configDir/citadel.pid.
func WritePID(configDir string) error {
	return os.WriteFile(
		filepath.Join(configDir, "citadel.pid"),
		[]byte(itoa(os.Getpid())),
		0600,
	)
}

// RemovePID removes the PID file.
func RemovePID(configDir string) {
	os.Remove(filepath.Join(configDir, "citadel.pid"))
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}

		s.mu.Lock()
		s.clients = append(s.clients, conn)
		s.mu.Unlock()

		go s.handleClient(conn)
	}
}

// handleClient spawns a fresh PTY for the attached client and relays I/O.
// The protocol is simple: raw bytes in both directions, with a 4-byte
// resize message prefix (cols uint16 LE, rows uint16 LE) when the client
// sends exactly 4 bytes starting with 0x00.
func (s *Server) handleClient(conn net.Conn) {
	defer func() {
		conn.Close()
		s.removeClient(conn)
	}()

	session, err := console.NewPTYSession(console.PTYConfig{
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		log.Printf("[instance] failed to create PTY for attached client: %v", err)
		return
	}
	defer session.Close()

	relaySession(conn, session)
}

// ptyIO is the subset of *console.PTYSession that relaySession needs. It exists
// so the relay teardown (the part that had the Ctrl-D-hang bug) is unit-testable
// with a fake session and a net.Pipe, without spawning a real shell/PTY.
type ptyIO interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
}

// relaySession pipes conn<->session bidirectionally until either side ends.
//
// CRITICAL teardown invariant: when the shell exits (session.Read returns EOF),
// the PTY->client goroutine MUST close conn. The client->PTY loop below is
// blocked on conn.Read, and the attached client is blocked on its own conn.Read;
// nothing else closes conn. Without this, a Ctrl-D that exits the shell left the
// attach session hung forever (the reported freeze). Closing conn unblocks both
// this loop and the client, so both detach cleanly. Double-close is harmless.
func relaySession(conn net.Conn, session ptyIO) {
	// PTY -> client. On exit (shell died / PTY EOF, or a write to a detached
	// client), close conn so the client->PTY loop and the remote client tear down.
	go func() {
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := session.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// client -> PTY (with resize detection).
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data := buf[:n]
			// Resize message: 0x00 + 4 bytes (cols_le16 + rows_le16)
			if n == 5 && data[0] == 0x00 {
				cols := binary.LittleEndian.Uint16(data[1:3])
				rows := binary.LittleEndian.Uint16(data[3:5])
				if cols > 0 && rows > 0 {
					_ = session.Resize(cols, rows)
				}
				continue
			}
			if _, werr := session.Write(data); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) removeClient(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.clients {
		if c == conn {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			return
		}
	}
}

// Close shuts down the server and removes the socket.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	clients := make([]net.Conn, len(s.clients))
	copy(clients, s.clients)
	s.mu.Unlock()

	for _, c := range clients {
		c.Close()
	}

	s.ln.Close()
	os.Remove(s.sockPath)
}

// SendSIGWINCH sends SIGWINCH to the current process (used by the client
// to trigger a resize propagation when connecting).
func SendSIGWINCH() {
	p, _ := os.FindProcess(os.Getpid())
	if p != nil {
		_ = p.Signal(syscall.SIGWINCH)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ReadPIDFile reads and returns the PID from configDir/citadel.pid.
func ReadPIDFile(configDir string) int {
	return PID(configDir)
}
