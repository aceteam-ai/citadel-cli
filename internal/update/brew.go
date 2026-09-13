// internal/update/brew.go
//
// Homebrew-awareness for macOS binary updates (citadel-cli#1043).
//
// A Homebrew install lays the citadel binary out as a STABLE symlink
// (<prefix>/bin/citadel) pointing into a VERSIONED Cellar directory
// (<prefix>/Cellar/citadel/<version>/bin/citadel). os.Executable +
// EvalSymlinks resolves that symlink to the Cellar path, so an in-place binary
// swap would overwrite the file Homebrew tracks in its Cellar receipt --
// corrupting Homebrew's state and setting up a `brew upgrade`/`brew doctor`
// conflict later. On such a node the correct update mechanism is `brew
// upgrade`, not our own atomic swap.
//
// The functions here are pure (no I/O beyond the one CurrentBinaryIsHomebrewManaged
// convenience) so the brew-vs-in-place decision and the stable-path resolution
// are unit-testable on any platform by injecting the resolved path / GOOS /
// GOARCH rather than needing a real Homebrew install.
package update

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// brewBinaryName is the citadel binary basename Homebrew installs.
const brewBinaryName = "citadel"

// ErrHomebrewManaged is returned by ApplyUpdate / Rollback when the running
// binary is Homebrew-managed on macOS: an in-place swap would corrupt
// Homebrew's Cellar receipt, so the update must go through `brew upgrade`
// instead. It is a sentinel so callers can branch on it (errors.Is) to render
// brew-specific guidance rather than a generic failure.
var ErrHomebrewManaged = errors.New("citadel is Homebrew-managed; update it with `brew upgrade citadel` instead of an in-place binary swap")

// HomebrewPrefixForArch returns the default Homebrew prefix for a GOARCH:
// Apple Silicon (arm64) uses /opt/homebrew, Intel (amd64) uses /usr/local.
// This is a hint for messaging/documentation; the actual update and
// stable-path decisions derive the prefix from the resolved binary path (see
// homebrewPrefixFromCellarPath) so they stay correct for an amd64 binary
// running under Rosetta on an arm64 Mac (which can sit under either prefix).
func HomebrewPrefixForArch(goarch string) string {
	if goarch == "arm64" {
		return "/opt/homebrew"
	}
	return "/usr/local"
}

// HomebrewBinPath returns the stable Homebrew symlink path for the citadel
// binary under prefix (e.g. /opt/homebrew/bin/citadel). Unlike the versioned
// Cellar path EvalSymlinks resolves to, this path is stable across `brew
// upgrade` (Homebrew repoints the symlink), so it is the right ExecPath to bake
// into a launchd plist.
func HomebrewBinPath(prefix string) string {
	return filepath.Join(prefix, "bin", brewBinaryName)
}

// homebrewPrefixFromCellarPath extracts the Homebrew prefix from a resolved
// binary path that lives under a Cellar (e.g.
// /opt/homebrew/Cellar/citadel/2.1.0/bin/citadel -> /opt/homebrew). ok is false
// when the path is not under any Cellar. macOS paths are always "/"-separated,
// so the marker is hardcoded rather than derived from filepath.Separator (which
// would be "\" on a Windows build of this cross-platform file).
func homebrewPrefixFromCellarPath(resolved string) (string, bool) {
	const marker = "/Cellar/"
	idx := strings.Index(resolved, marker)
	if idx <= 0 {
		return "", false
	}
	return resolved[:idx], true
}

// StableDarwinExecPath decides the ExecPath to bake into a launchd plist for a
// binary whose resolved (symlink-followed) path is `resolved`. When the binary
// is Homebrew-managed (lives under a Cellar), it returns the stable
// <prefix>/bin/citadel symlink and brew=true, so the service keeps working
// across `brew upgrade`. Otherwise it returns `resolved` unchanged and
// brew=false (e.g. a curl|bash install into ~/.local/bin or /usr/local/bin,
// which is already a stable plain file, not a Cellar symlink).
func StableDarwinExecPath(resolved string) (path string, brew bool) {
	if prefix, ok := homebrewPrefixFromCellarPath(resolved); ok {
		return HomebrewBinPath(prefix), true
	}
	return resolved, false
}

// IsHomebrewManagedPath reports whether a resolved binary path is a
// Homebrew-managed install that must NOT be swapped in place. It is true only
// on macOS AND when the resolved path lives under a Cellar -- this is what
// separates a Homebrew install (a <prefix>/bin symlink into the Cellar) from a
// curl|bash install that happens to sit at the same <prefix>/bin location on an
// Intel Mac (a plain file whose resolved path contains no Cellar segment).
func IsHomebrewManagedPath(resolved, goos string) bool {
	if goos != "darwin" {
		return false
	}
	_, ok := homebrewPrefixFromCellarPath(resolved)
	return ok
}

// CurrentBinaryIsHomebrewManaged reports whether the currently-running binary
// is a Homebrew-managed install on macOS. It is the impure convenience wrapper
// over IsHomebrewManagedPath used by the update paths (ApplyUpdate, the
// auto-updater, the AGENT_UPDATE handler, `citadel update install`). A path
// that cannot be resolved fails closed to "not managed" so the normal in-place
// path proceeds (the ApplyUpdate guard is the backstop either way).
func CurrentBinaryIsHomebrewManaged() bool {
	p, err := GetCurrentBinaryPath()
	if err != nil {
		return false
	}
	return IsHomebrewManagedPath(p, runtime.GOOS)
}
