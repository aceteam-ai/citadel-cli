// internal/terminal/tmux.go
package terminal

import "github.com/aceteam-ai/citadel-cli/internal/tmux"

// sessionCommand returns the program + args the PTY should run for a connection.
//
// When the server is configured with a SessionName and a usable tmux binary is
// available, it returns a `tmux new-session -A -s <name>` invocation so the
// connection attaches to (or creates) a persistent named session that survives
// reconnects. Otherwise it returns nil, signalling the caller to fall back to a
// bare shell.
//
// The returned command starts only a shell inside tmux; launching claude (or
// any agent) is a separate explicit step and never coupled here.
func sessionCommand(sessionName, shell string) []string {
	if sessionName == "" {
		return nil
	}
	if err := tmux.ValidateSessionName(sessionName); err != nil {
		return nil
	}
	bin, err := tmux.Resolve()
	if err != nil {
		return nil
	}
	return append([]string{bin}, tmux.AttachOrCreateArgs(sessionName, shell)...)
}
