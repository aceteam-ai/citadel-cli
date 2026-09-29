package tmux

// PersistentSessionCommand moves a tmux create or attach command into a
// service-manager scope when the platform can do so safely. A tmux server is a
// long-lived child: if Citadel is the process that starts it, leaving it in the
// citadel.service control group makes systemd kill every persistent session on
// a worker restart.
//
// Unsupported platforms and processes not running under a compatible service
// manager receive the original command unchanged. The returned slice is always
// a fresh allocation so callers can safely retain or modify their input.
func PersistentSessionCommand(sessionName string, command []string) []string {
	copyOf := append([]string(nil), command...)
	if len(copyOf) == 0 {
		return copyOf
	}
	if err := ValidateSessionName(sessionName); err != nil {
		return copyOf
	}
	return persistentSessionCommand(sessionName, copyOf)
}

func identitySessionCommand(_ string, command []string) []string {
	return append([]string(nil), command...)
}
