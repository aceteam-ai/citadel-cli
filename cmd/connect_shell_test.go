package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// TestConnectIsShellTarget locks the routing discriminator that decides whether
// a `citadel connect <arg>` invocation is a remote shell (bare name/IP) or the
// existing raw-TCP pipe (host:port). The IPv6 cases guard the all-colons trap:
// tsnet assigns IPv6 addresses that must not be misread as host:port.
func TestConnectIsShellTarget(t *testing.T) {
	cases := []struct {
		arg   string
		shell bool
	}{
		{"gpu-node-1", true},           // bare hostname -> shell
		{"ubuntu-gpu", true},           // bare hostname -> shell
		{"100.64.0.5", true},           // bare IPv4 -> shell
		{"192.168.2.10", true},         // bare LAN IPv4 -> shell
		{"fd7a:115c:a1e0::1", true},    // bare IPv6 (all colons) -> shell, not host:port
		{"gpu-node-1:5432", false},     // host:port -> raw TCP
		{"100.64.0.5:11434", false},    // ip:port -> raw TCP
		{"[fd7a:115c:a1e0::1]:22", false}, // bracketed IPv6 with port -> raw TCP
	}
	for _, c := range cases {
		if got := connectIsShellTarget(c.arg); got != c.shell {
			t.Errorf("connectIsShellTarget(%q) = %v, want %v", c.arg, got, c.shell)
		}
	}
}

// TestRemoteShellProtocolRoundtrip verifies the client encodes and decodes the
// same wire protocol the terminal server speaks (internal/terminal.Message), so
// input/resize/pong the client sends and output the server sends survive a
// marshal/unmarshal roundtrip with payloads and dimensions intact.
func TestRemoteShellProtocolRoundtrip(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		data, err := terminal.NewInputMessage([]byte("ls -la\n")).Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		msg, err := terminal.UnmarshalMessage(data)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != terminal.MessageTypeInput || string(msg.Payload) != "ls -la\n" {
			t.Fatalf("input roundtrip mismatch: type=%q payload=%q", msg.Type, msg.Payload)
		}
	})

	t.Run("resize", func(t *testing.T) {
		data, err := terminal.NewResizeMessage(120, 40).Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		msg, err := terminal.UnmarshalMessage(data)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != terminal.MessageTypeResize || msg.Cols != 120 || msg.Rows != 40 {
			t.Fatalf("resize roundtrip mismatch: type=%q cols=%d rows=%d", msg.Type, msg.Cols, msg.Rows)
		}
	})

	t.Run("output", func(t *testing.T) {
		data, err := terminal.NewOutputMessage([]byte("hello\r\n")).Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		msg, err := terminal.UnmarshalMessage(data)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != terminal.MessageTypeOutput || string(msg.Payload) != "hello\r\n" {
			t.Fatalf("output roundtrip mismatch: type=%q payload=%q", msg.Type, msg.Payload)
		}
	})
}
