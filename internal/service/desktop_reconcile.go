package service

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DesktopReconcileStatus is deliberately small and contains no node identity,
// service output, filesystem paths, or credentials. The desktop UI may show it.
type DesktopReconcileStatus string

const (
	DesktopStaged        DesktopReconcileStatus = "staged"
	DesktopUnmanaged     DesktopReconcileStatus = "unmanaged"
	DesktopUnchanged     DesktopReconcileStatus = "unchanged"
	DesktopRestarted     DesktopReconcileStatus = "restarted"
	DesktopStageFailed   DesktopReconcileStatus = "stage_failed"
	DesktopInspectFailed DesktopReconcileStatus = "inspect_failed"
	DesktopRestartFailed DesktopReconcileStatus = "restart_failed"
)

// ReconcileDesktopHelper stages the app's current helper without touching node
// enrollment/config. It rewrites only this user's launchd job if that job
// already points to a bundled or private desktop helper. restart is called
// once, only after a changed plist or a missing current helper is repaired.
func ReconcileDesktopHelper(source, home string, restart func(string) error) (DesktopReconcileStatus, error) {
	current, installed, err := materializeDesktopHelper(source, home)
	if err != nil {
		return DesktopStageFailed, err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	info, err := os.Lstat(plistPath)
	if os.IsNotExist(err) {
		return DesktopStaged, nil
	}
	if err != nil {
		return DesktopInspectFailed, err
	}
	if !info.Mode().IsRegular() {
		return DesktopUnmanaged, nil
	}
	content, err := os.ReadFile(plistPath)
	if err != nil {
		return DesktopInspectFailed, err
	}
	args := parseLaunchdProgramArguments(string(content))
	if !isDesktopManagedPlist(string(content), args, home) {
		return DesktopUnmanaged, nil
	}
	desired := []string{current, "--no-auto-update", "work"}
	changed := len(args) != len(desired)
	if !changed {
		for i := range desired {
			if args[i] != desired[i] {
				changed = true
				break
			}
		}
	}
	if changed {
		next, ok := replaceLaunchdProgramArguments(string(content), desired)
		if !ok {
			return DesktopInspectFailed, fmt.Errorf("cannot update desktop launchd arguments")
		}
		if err := writeDesktopPlist(plistPath, next); err != nil {
			return DesktopInspectFailed, err
		}
	}
	if !changed && !installed {
		return DesktopUnchanged, nil
	}
	if err := restart(plistPath); err != nil {
		return DesktopRestartFailed, err
	}
	return DesktopRestarted, nil
}

func isDesktopManagedPlist(content string, args []string, home string) bool {
	if label, ok := launchdPlistLabel(content); !ok || label != launchdLabel {
		return false
	}
	if strings.Count(content, "<key>ProgramArguments</key>") != 1 {
		return false
	}
	if len(args) != 2 && len(args) != 3 {
		return false
	}
	if !desktopManagedExec(args[0], home) {
		return false
	}
	return (len(args) == 2 && args[1] == "work") ||
		(len(args) == 3 && args[1] == "--no-auto-update" && args[2] == "work")
}

func desktopManagedExec(path, home string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	if bundledDesktopHelper(path) {
		return true
	}
	base := filepath.Join(home, "Library", "Application Support", "ai.aceteam.citadel", "helpers")
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[1] != "citadel" || len(parts[0]) != 64 {
		return false
	}
	for _, c := range parts[0] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// launchdPlistLabel parses the XML label rather than searching for a string
// that could appear in a comment or unrelated plist value.
func launchdPlistLabel(content string) (string, bool) {
	decoder := xml.NewDecoder(strings.NewReader(content))
	var label string
	var found bool
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return label, found
		}
		if err != nil {
			return "", false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return "", false
		}
		if key != "Label" {
			continue
		}
		if found {
			return "", false
		}
		for {
			token, err = decoder.Token()
			if err != nil {
				return "", false
			}
			if valueStart, ok := token.(xml.StartElement); ok {
				if valueStart.Name.Local != "string" {
					return "", false
				}
				var value string
				if err := decoder.DecodeElement(&value, &valueStart); err != nil {
					return "", false
				}
				label, found = value, true
				break
			}
		}
	}
}

func replaceLaunchdProgramArguments(content string, args []string) (string, bool) {
	idx := strings.Index(content, "<key>ProgramArguments</key>")
	if idx < 0 {
		return "", false
	}
	startRel := strings.Index(content[idx:], "<array>")
	if startRel < 0 {
		return "", false
	}
	start := idx + startRel + len("<array>")
	endRel := strings.Index(content[start:], "</array>")
	if endRel < 0 {
		return "", false
	}
	end := start + endRel
	var values strings.Builder
	values.WriteString("\n")
	for _, arg := range args {
		values.WriteString("        <string>")
		values.WriteString(xmlEscape(arg))
		values.WriteString("</string>\n")
	}
	values.WriteString("    ")
	return content[:start] + values.String() + content[end:], true
}

func writeDesktopPlist(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".citadel-reconcile-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.WriteString(tmp, content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
