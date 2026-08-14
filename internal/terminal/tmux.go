// internal/terminal/tmux.go
package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
	"github.com/aceteam-ai/citadel-cli/internal/tmuxinstall"
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
// tmux backing to be turned off. An empty (or whitespace-only) name means no
// session was configured, which is the off-by-default case, so it is treated as
// disabled. The explicit sentinels ("none"/"off"/...) remain supported for
// operators who opt out after setting a name.
func sessionDisabled(sessionName string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(sessionName))
	return trimmed == "" || disableSentinels[trimmed]
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

// resolveSessionOverride decides the effective session BASE name for a single
// connection (citadel #759): configDefault is the node's own configured
// default (Config.SessionName); requestOverride is read from the connection
// request's "session" query parameter, sent only by the CLI connect/ssh path
// ("none" for a bare shell, or a session name for --tmux). The web console
// and any other caller that sends no override at all get configDefault
// completely unchanged: that is what keeps this additive rather than a
// behavior change for existing callers. A present override, even "none",
// always wins over configDefault: only the CLI sends one, so honoring it can
// never surprise a browser connection.
func resolveSessionOverride(configDefault, requestOverride string) (base string, overridden bool) {
	if requestOverride == "" {
		return configDefault, false
	}
	return requestOverride, true
}

// resolveSessionCommand is handleWebSocket's full session decision, factored
// out so it is unit-testable without a live WebSocket/PTY: it applies the
// per-connection override over the node default (resolveSessionOverride),
// derives the per-user session name, and, ONLY when an override explicitly
// asked for a persistent session that isn't currently backed by a resolvable
// tmux binary, attempts an on-node install via ensureInstall before falling
// back to a bare shell (citadel #759). The plain (non-overridden) default
// path is untouched: a node whose own CITADEL_TERMINAL_SESSION wants tmux but
// has none installed still just falls back, exactly as before this change.
//
// command is nil for a bare shell. wantedSession reports whether a
// persistent session was asked for at all (by the override or the node
// default), which is what the caller needs to decide whether the "tmux
// unavailable" warning applies.
func resolveSessionCommand(configDefault, requestOverride, userID, shell string, ensureInstall func() bool) (command []string, sessionName string, wantedSession bool) {
	base, overridden := resolveSessionOverride(configDefault, requestOverride)
	if sessionDisabled(base) {
		return nil, "", false
	}

	sessionName = sessionNameForUser(base, userID)
	command = sessionCommand(sessionName, shell)
	if command == nil && overridden && ensureInstall != nil {
		if ensureInstall() {
			command = sessionCommand(sessionName, shell)
		}
	}
	return command, sessionName, true
}

// tmuxInstallTimeout bounds the on-demand install attempt (below) well under
// the terminal server's http.Server WriteTimeout (15s, see Start in
// server.go): a slow or stalled download must degrade to the bare-shell
// fallback rather than trip the server's own write deadline and kill the
// WebSocket upgrade outright.
const tmuxInstallTimeout = 10 * time.Second

// ensureTmuxInstalledFn attempts an on-node install of a Citadel-managed tmux
// binary (mirrors 'citadel tmux install', cmd/tmux.go, both built on
// internal/tmuxinstall) when a per-connection override explicitly requests a
// persistent session but no tmux binary resolves (citadel #759), so
// '--tmux' works without the operator installing tmux by hand. Package var so
// tests can substitute a fake without a live network fetch. Returns true when
// a tmux binary is now present (already installed counts).
var ensureTmuxInstalledFn = defaultEnsureTmuxInstalled

func defaultEnsureTmuxInstalled() bool {
	inst := tmuxinstall.New(tmuxinstall.WithHTTPClient(&http.Client{Timeout: tmuxInstallTimeout}))
	installed, err := inst.Ensure()
	return err == nil && installed
}
