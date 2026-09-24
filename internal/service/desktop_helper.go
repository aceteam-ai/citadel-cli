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
	if !bundledDesktopHelper(source) {
		return "", fmt.Errorf("not a bundled desktop helper")
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("desktop helper must be a regular file")
	}
	base := filepath.Join(home, "Library", "Application Support", "ai.aceteam.citadel", "helpers")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(base, ".staging-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, "citadel")
	output, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
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
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	version := hex.EncodeToString(hash.Sum(nil))
	targetDir := filepath.Join(base, version)
	target := filepath.Join(targetDir, "citadel")
	if existingInfo, err := os.Lstat(target); err == nil {
		if !existingInfo.Mode().IsRegular() {
			return "", fmt.Errorf("installed desktop helper is not a regular file")
		}
		existing, err := os.Open(target)
		if err != nil {
			return "", err
		}
		defer existing.Close()
		other := sha256.New()
		if _, err := io.Copy(other, existing); err != nil || !strings.EqualFold(hex.EncodeToString(other.Sum(nil)), version) {
			return "", fmt.Errorf("installed desktop helper differs from bundled version")
		}
		if err := os.Chmod(target, 0o700); err != nil {
			return "", err
		}
		return target, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(tmp, targetDir); err != nil {
		return "", err
	}
	return target, nil
}
