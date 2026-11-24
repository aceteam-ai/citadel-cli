//go:build !windows && !darwin

package service

// EnableUserAutostart is Windows-only.
func EnableUserAutostart(exe string) error { return ErrUserAutostartUnsupported }

// DisableUserAutostart is Windows-only.
func DisableUserAutostart() error { return ErrUserAutostartUnsupported }

// UserAutostart is Windows-only.
func UserAutostart(thisExe string) (UserAutostartStatus, error) {
	return UserAutostartStatus{}, ErrUserAutostartUnsupported
}
