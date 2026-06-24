// Package tmuxinstall provisions a Citadel-managed static tmux binary into the
// path that the internal/tmux package resolves (tmux.ManagedBinaryPath), so that
// persistent tmux-backed terminal sessions (aceteam EPIC #4144, issues #302/#304)
// work on nodes that lack a system tmux, without manual setup.
//
// # Sourcing strategy
//
// tmux is not statically linkable in the trivial sense: it depends on libevent
// and ncurses, and neither Apple's toolchain (macOS) nor stock Windows can
// produce a genuinely static, self-contained tmux. There is also no upstream
// "official" static tmux release to point at. To stay honest about provenance we
// deliberately do NOT pin third-party / personal GitHub mirrors (their artifacts
// cannot be vetted and would violate our supply-chain constraints).
//
// Instead, the managed binary is sourced from Citadel-org-controlled artifacts:
// static tmux builds produced in CI and attached to citadel-cli GitHub releases
// (or a managed mirror), fetched and SHA-256-verified with the exact same trust
// model as the existing auto-updater (internal/update). An entry is only treated
// as installable when it carries a real, confirmed SHA-256 checksum; an empty
// checksum marks the platform as GATED and Resolve()/Install refuse to download
// anything for it (so we can never install an unverified binary).
//
// This file is the single source table. As org-built static tmux artifacts land,
// fill in URL + SHA256 (+ archive format) for the corresponding platform and the
// plumbing below will provision it. Until then every platform is gated and the
// terminal server keeps its existing, safe fallback to a bare (non-persistent)
// shell.
package tmuxinstall

import (
	"fmt"
	"runtime"
)

// archiveFormat describes how the downloaded artifact is packaged so the
// installer knows how to extract the tmux binary from it.
type archiveFormat string

const (
	// formatRaw means the downloaded file is the tmux executable itself, with
	// no container. It is written directly to the managed path.
	formatRaw archiveFormat = "raw"
	// formatGzip means the downloaded file is a single gzip-compressed
	// executable (e.g. tmux.gz); it is gunzipped to the managed path.
	formatGzip archiveFormat = "gzip"
	// formatTarGz means the downloaded file is a .tar.gz containing a "tmux"
	// (or "tmux.exe") entry, which is extracted to the managed path.
	formatTarGz archiveFormat = "tar.gz"
	// formatZip means the downloaded file is a .zip containing a "tmux"
	// (or "tmux.exe") entry, which is extracted to the managed path.
	formatZip archiveFormat = "zip"
)

// Source describes where to obtain a Citadel-managed static tmux binary for a
// specific OS/arch and how to verify it.
//
// A Source is "vetted" (installable) only when both URL and SHA256 are non-empty.
// When SHA256 is empty the platform is GATED: no download is attempted, because
// we never install a binary we cannot checksum-verify. This is enforced in
// (Source).vetted and surfaced by Available / Install.
type Source struct {
	// URL is the download location of the artifact. Must be HTTPS and point at a
	// Citadel-org-controlled artifact (citadel-cli release asset or managed
	// mirror) — never an unvetted third-party / personal mirror.
	URL string
	// SHA256 is the lowercase hex SHA-256 of the downloaded artifact (the file at
	// URL, before extraction). Empty means the platform is gated.
	SHA256 string
	// Format is how the artifact is packaged (see archiveFormat).
	Format archiveFormat
	// Note documents provenance / why a platform is gated. Surfaced in errors.
	Note string
}

// vetted reports whether this source can actually be installed: it must have a
// URL and a checksum. A missing checksum gates the platform.
func (s Source) vetted() bool {
	return s.URL != "" && s.SHA256 != ""
}

// platformKey is the "<goos>/<goarch>" map key for the source table.
func platformKey(goos, goarch string) string {
	return goos + "/" + goarch
}

// sources is the per-platform source+checksum table.
//
// IMPORTANT: every entry is currently GATED (empty SHA256) because no vetted,
// Citadel-org-built static tmux artifact has been published yet. Filling in a
// real URL + SHA256 for a platform (and confirming the checksum against the
// actual artifact) is what flips it to installable. Do NOT invent checksums.
//
//	| platform        | status | source / notes                                   |
//	|-----------------|--------|--------------------------------------------------|
//	| linux/amd64     | GATED  | org-built static tmux (libevent+ncurses) pending |
//	| linux/arm64     | GATED  | org-built static tmux pending                    |
//	| darwin/amd64    | GATED  | no static macOS build; resolve via `brew tmux`   |
//	| darwin/arm64    | GATED  | no static macOS build; resolve via `brew tmux`   |
//	| windows/amd64   | GATED  | no native static tmux (needs Cygwin/MSYS2)       |
//
// linux/amd64 and linux/arm64 are the only realistically vettable targets and
// are the intended first entries to be filled in by CI. macOS and Windows are
// expected to remain gated; on those platforms the terminal server falls back to
// a bare shell (and macOS users can `brew install tmux`, which Resolve picks up
// via PATH).
var sources = map[string]Source{
	platformKey("linux", "amd64"): {
		URL:    "",
		SHA256: "",
		Format: formatTarGz,
		Note:   "org-built static tmux (libevent+ncurses) for linux/amd64 not yet published; gated until a CI artifact + verified SHA-256 exists (see #304)",
	},
	platformKey("linux", "arm64"): {
		URL:    "",
		SHA256: "",
		Format: formatTarGz,
		Note:   "org-built static tmux for linux/arm64 not yet published; gated until a CI artifact + verified SHA-256 exists (see #304)",
	},
	platformKey("darwin", "amd64"): {
		URL:    "",
		SHA256: "",
		Format: formatTarGz,
		Note:   "macOS cannot produce a fully static tmux; gated. Install tmux via Homebrew (`brew install tmux`) and it will be picked up on PATH",
	},
	platformKey("darwin", "arm64"): {
		URL:    "",
		SHA256: "",
		Format: formatTarGz,
		Note:   "macOS cannot produce a fully static tmux; gated. Install tmux via Homebrew (`brew install tmux`) and it will be picked up on PATH",
	},
	platformKey("windows", "amd64"): {
		URL:    "",
		SHA256: "",
		Format: formatZip,
		Note:   "Windows has no native static tmux (requires Cygwin/MSYS2); gated. Persistent sessions fall back to a bare shell on Windows",
	},
}

// SourceFor returns the Source for the given OS/arch and whether the table has an
// entry at all. A returned-but-gated source (ok==true, vetted()==false) means we
// know about the platform but have no verified artifact for it yet.
func SourceFor(goos, goarch string) (Source, bool) {
	s, ok := sources[platformKey(goos, goarch)]
	return s, ok
}

// CurrentSource returns the Source for the platform the binary is running on.
func CurrentSource() (Source, bool) {
	return SourceFor(runtime.GOOS, runtime.GOARCH)
}

// Available reports whether a vetted (checksum-verified) managed tmux artifact
// exists for the current platform — i.e. whether Install can do anything other
// than return a "gated"/"unsupported" error. It does not perform any network or
// filesystem I/O.
func Available() bool {
	s, ok := CurrentSource()
	return ok && s.vetted()
}

// gatedError returns a descriptive, actionable error for a platform that has no
// installable artifact (either unknown or gated).
func gatedError(goos, goarch string) error {
	if s, ok := SourceFor(goos, goarch); ok {
		return fmt.Errorf("managed tmux is not available for %s/%s: %s", goos, goarch, s.Note)
	}
	return fmt.Errorf("managed tmux is not supported on %s/%s: no source entry; install tmux via your package manager so it is found on PATH", goos, goarch)
}
