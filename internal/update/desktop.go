package update

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrDesktopManaged marks helpers whose lifecycle belongs to the desktop app.
var ErrDesktopManaged = errors.New("Citadel desktop manages this helper; update the app to update its node helper")

// IsDesktopManagedPath covers both a helper inside any translocated or moved
// .app and the private, versioned copy installed for launchd by that app.
func IsDesktopManagedPath(path, goos string) bool {
	if goos != "darwin" {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	name := filepath.Base(clean)
	if name != "citadel" && !strings.HasPrefix(name, "citadel-") {
		return false
	}
	return strings.Contains(clean, ".app/Contents/") ||
		strings.Contains(clean, "/Library/Application Support/ai.aceteam.citadel/helpers/")
}

func CurrentBinaryIsDesktopManaged() bool {
	path, err := os.Executable()
	if err != nil {
		return false
	}
	return IsDesktopManagedPath(path, runtime.GOOS)
}
