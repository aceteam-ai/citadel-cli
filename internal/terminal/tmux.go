// internal/terminal/tmux.go
package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
)

// disableSentinels are case-insensitive values of CITADEL_TERMINAL_SESSION (or
// Config.SessionName) that explicitly turn persistent tmux backing OFF, forcing
// a bare, non-persistent shell.
var disableSentinels = map[string]bool{
	"none":     true,
	"off":      true,
	"disabled": true,
	"false":    true,
	"0":        true,
}

// sessionDisabled reports whether the configured session base name asks for
// tmux backing to be turned off.
func sessionDisabled(sessionName string) bool {
	return disableSentinels[strings.ToLower(strings.TrimSpace(sessionName))]
}

// sessionNameForUser derives a stable, validated tmux session name from a base
// name and a user ID. The same (base, userID) pair always yields the same name,
// which is what lets a reconnecting client re-attach to the user's existing
// persistent session. Different users get different names, so they never share
// a terminal.
//
// userIDs are not guaranteed to be tmux-safe (UUIDs are fine, but emails carry
// '@' and '.', which tmux uses to address windows/panes). We therefore keep
// only the safe characters from the user ID and, when sanitisation would change
// or empty it, append a short hash of the original so distinct users can never
// collide onto the same session. The result is always within tmux's length
// limit and passes tmux.ValidateSessionName.
//
// An empty userID (or a base that already fails validation) falls back to the
// base name alone so a session is still persistent, just shared.
func sessionNameForUser(base, userID string) string {
	if userID == "" {
		return base
	}

	var safe strings.Builder
	for _, r := range userID {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			safe.WriteRune(r)
		}
	}
	cleaned := safe.String()

	// If sanitisation dropped any characters, the cleaned form is ambiguous
	// (two different user IDs could clean to the same string), so disambiguate
	// with a short hash of the original ID.
	if cleaned != userID {
		sum := sha256.Sum256([]byte(userID))
		cleaned = cleaned + "-" + hex.EncodeToString(sum[:])[:8]
	}

	name := base + "-" + cleaned

	// tmux session names are capped at 64 chars (see tmux.ValidateSessionName).
	// If the combined name is too long, fall back to base + a hash so the name
	// stays bounded and deterministic.
	if err := tmux.ValidateSessionName(name); err != nil {
		sum := sha256.Sum256([]byte(userID))
		name = base + "-" + hex.EncodeToString(sum[:])[:16]
		if err := tmux.ValidateSessionName(name); err != nil {
			// base itself is invalid or too long; give up on per-user naming.
			return base
		}
	}
	return name
}

// sessionCommand returns the program + args the PTY should run for a connection.
//
// When the server is configured with a SessionName and a usable tmux binary is
// available, it returns a `tmux new-session -A -s <name>` invocation so the
// connection attaches to (or creates) a persistent named session that survives
// reconnects. Otherwise it returns nil, signalling the caller to fall back to a
// bare shell.
//
// A SessionName matching a disable sentinel ("none"/"off"/...) returns nil so
// operators can opt out of persistence without unsetting the default.
//
// The returned command starts only a shell inside tmux; launching claude (or
// any agent) is a separate explicit step and never coupled here.
func sessionCommand(sessionName, shell string) []string {
	if sessionName == "" || sessionDisabled(sessionName) {
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
