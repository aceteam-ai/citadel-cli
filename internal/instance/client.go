// client.go provides the attach client for connecting to a running Citadel instance.
//
//go:build !windows

package instance

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// Attach connects to a running Citadel instance via Unix socket and relays
// stdin/stdout as a raw terminal. Returns when the session ends or Ctrl+]
// is pressed. The caller's terminal is put into raw mode for the duration.
func Attach(configDir string) error {
	sockPath := SocketPath(configDir)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("failed to connect to running instance: %w", err)
	}
	defer conn.Close()

	pid := PID(configDir)
	if pid > 0 {
		fmt.Fprintf(os.Stderr, "Citadel is already running (PID %d). Starting a new shell on the running instance...\n", pid)
	} else {
		fmt.Fprintln(os.Stderr, "Citadel is already running. Starting a new shell on the running instance...")
	}
	fmt.Fprintln(os.Stderr, "Press Ctrl+] to detach.")
	fmt.Fprintln(os.Stderr, "")

	// Put terminal in raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Send initial window size
	sendResize(conn)

	// Handle SIGWINCH for terminal resize
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	go func() {
		for range sigCh {
			sendResize(conn)
		}
	}()

	// Socket → stdout
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// stdin → socket (with Ctrl+] detection)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				for i := 0; i < n; i++ {
					if buf[i] == 0x1d { // Ctrl+]
						conn.Close()
						return
					}
				}
				conn.Write(buf[:n])
			}
			if err != nil {
				conn.Close()
				return
			}
		}
	}()

	<-done
	fmt.Fprintln(os.Stderr, "\nDetached from Citadel.")
	return nil
}

// sendResize sends the current terminal dimensions over the socket.
func sendResize(conn net.Conn) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w == 0 || h == 0 {
		return
	}
	var msg [5]byte
	msg[0] = 0x00 // resize marker
	binary.LittleEndian.PutUint16(msg[1:3], uint16(w))
	binary.LittleEndian.PutUint16(msg[3:5], uint16(h))
	conn.Write(msg[:])
}
