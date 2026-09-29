//go:build !linux

package tmux

func persistentSessionCommand(_ string, command []string) scopeDecision {
	return scopeDecision{command: command, reason: "transient systemd scopes are only supported on Linux"}
}
