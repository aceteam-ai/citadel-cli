package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// bundledDesktopHelper recognizes the binary Tauri puts inside a macOS app,
// including Gatekeeper App Translocation and apps moved after installation.
func bundledDesktopHelper(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	name := filepath.Base(clean)
	return (name == "citadel" || strings.HasPrefix(name, "citadel-")) &&
		strings.Contains(clean, ".app/Contents/")
}

// installDesktopHelper copies a bundled helper to a private, content-versioned
// location. launchd refers to this copy, so ejecting a DMG, moving the app, or
// App Translocation cannot invalidate its ProgramArguments.
func installDesktopHelper(source, home string) (string, error) {
	path, _, err := materializeDesktopHelper(source, home)
	return path, err
}

// materializeDesktopHelper reports whether it had to create the content-addressed
// copy. A missing copy at an already-current launchd path requires a reload.
func materializeDesktopHelper(source, home string) (string, bool, error) {
	if !bundledDesktopHelper(source) {
		return "", false, fmt.Errorf("not a bundled desktop helper")
	}
	input, err := os.Open(source)
	if err != nil {
		return "", false, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("desktop helper must be a regular file")
	}
	base := filepath.Join(home, "Library", "Application Support", "ai.aceteam.citadel", "helpers")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return "", false, err
	}
	tmp, err := os.MkdirTemp(base, ".staging-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, "citadel")
	output, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", false, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	if copyErr == nil {
		copyErr = output.Chmod(0o700)
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return "", false, copyErr
	}
	if closeErr != nil {
		return "", false, closeErr
	}
	version := hex.EncodeToString(hash.Sum(nil))
	targetDir := filepath.Join(base, version)
	target := filepath.Join(targetDir, "citadel")
	if _, err := os.Lstat(target); err == nil {
		if err := verifyDesktopHelper(target, version); err != nil {
			return "", false, err
		}
		return target, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	if dirInfo, err := os.Lstat(targetDir); err == nil {
		if !dirInfo.IsDir() {
			return "", false, fmt.Errorf("installed desktop helper directory is invalid")
		}
		if err := os.Chmod(targetDir, 0o700); err != nil {
			return "", false, err
		}
		// The version directory may survive while its binary was removed.
		// Link the fully written staging file into it without replacing a file
		// created by another app instance in the meantime.
		if err := os.Link(staged, target); err != nil {
			if verifyErr := verifyDesktopHelper(target, version); verifyErr != nil {
				return "", false, err
			}
		}
		return target, true, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	if err := os.Rename(tmp, targetDir); err != nil {
		if verifyErr := verifyDesktopHelper(target, version); verifyErr != nil {
			return "", false, err
		}
	}
	return target, true, nil
}

func verifyDesktopHelper(path, version string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("installed desktop helper is not a regular file")
	}
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil || hex.EncodeToString(hash.Sum(nil)) != version {
		return fmt.Errorf("installed desktop helper differs from bundled version")
	}
	return os.Chmod(path, 0o700)
}
