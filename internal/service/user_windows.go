//go:build windows

package service

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// startupApprovedRunKey is where Settings > Apps > Startup and Task Manager
// record that the user switched a Run entry off. Citadel only reads it.
const startupApprovedRunKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`

// EnableUserAutostart writes the HKCU Run value so `citadel work --background`
// starts at the next logon. It is only ever called on an explicit request
// (`citadel worker autostart enable`), never at startup.
func EnableUserAutostart(exe string) error {
	value, err := BuildRunValue(exe)
	if err != nil {
		return err
	}
	return writeRunValue(UserAutostartValueName, value)
}

// DisableUserAutostart removes the Run value. Removing an absent value is not
// an error.
func DisableUserAutostart() error {
	return deleteRunValue(UserAutostartValueName)
}

// UserAutostart reports whether the Run value is set and whether it starts
// thisExe. It only reads the registry.
func UserAutostart(thisExe string) (UserAutostartStatus, error) {
	value, set, err := readRunValue(UserAutostartValueName)
	if err != nil {
		return UserAutostartStatus{}, err
	}
	st := autostartStatusFor(set, value, thisExe)
	if set {
		st.DisabledByUser = startupDisabledByUser(UserAutostartValueName)
	}
	return st, nil
}

func writeRunValue(name, data string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, UserRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open HKCU\\%s: %w", UserRunKeyPath, err)
	}
	defer k.Close()
	if err := k.SetStringValue(name, data); err != nil {
		return fmt.Errorf("write HKCU\\%s\\%s: %w", UserRunKeyPath, name, err)
	}
	return nil
}

func readRunValue(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, UserRunKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("open HKCU\\%s: %w", UserRunKeyPath, err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read HKCU\\%s\\%s: %w", UserRunKeyPath, name, err)
	}
	return v, true, nil
}

func deleteRunValue(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, UserRunKeyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open HKCU\\%s: %w", UserRunKeyPath, err)
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("delete HKCU\\%s\\%s: %w", UserRunKeyPath, name, err)
	}
	return nil
}

// startupDisabledByUser reports whether the user switched the entry off in
// Settings or Task Manager. The value's first byte is even when enabled and odd
// when disabled; an absent or unreadable value means enabled.
func startupDisabledByUser(name string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, startupApprovedRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	data, _, err := k.GetBinaryValue(name)
	if err != nil || len(data) == 0 {
		return false
	}
	return data[0]&1 == 1
}
