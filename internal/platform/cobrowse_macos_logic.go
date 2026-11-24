// internal/platform/cobrowse_macos_logic.go
//
// The decisions of the macOS headless co-browse runner (aceteam#13503 S10,
// citadel-cli#1635), kept free of build tags and of real processes so they run
// on any host: browser discovery over an injected probe, the session-dir sweep
// over injected process seams, and the identity checks that gate every kill.
// cobrowse_runner_darwin.go supplies the real probes (sysctl, ps, kill).
//
// Nothing here kills by bare PID. A process is touched only after its identity
// is proven: an owner by PID plus start time, a browser by its command line
// naming this session's exact profile dir plus being the leader of its own
// process group (the runner starts it with Setpgid).
package platform

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// macBrowserProbe is the injected view of a Mac that discovery reads.
type macBrowserProbe struct {
	getenv func(string) string
	// home is the user's home dir ("" when unknown); ~/Applications is skipped
	// without it.
	home string
	// isExec reports whether path is an existing, executable regular file.
	isExec func(string) bool
	// lookPath resolves a command on PATH.
	lookPath func(string) (string, error)
}

// macApp is one browser app bundle discovery knows.
type macApp struct {
	alias  string // value accepted from EnvCobrowseBrowser
	kind   string // BrowserInfo.Kind
	bundle string // "<bundle>.app" under an Applications dir
	exe    string // executable name inside Contents/MacOS
}

// macApps is the search order: Chrome, Edge, Chromium, then Chrome Canary.
var macApps = []macApp{
	{"chrome", "chrome", "Google Chrome", "Google Chrome"},
	{"edge", "edge", "Microsoft Edge", "Microsoft Edge"},
	{"chromium", "chromium", "Chromium", "Chromium"},
	{"canary", "chrome", "Google Chrome Canary", "Google Chrome Canary"},
}

// macPathNames are the PATH fallbacks, tried last.
var macPathNames = []string{"google-chrome", "chrome", "chromium", "microsoft-edge"}

// macAppBinary is the executable inside an app bundle under root.
func macAppBinary(root string, a macApp) string {
	return filepath.Join(root, a.bundle+".app", "Contents", "MacOS", a.exe)
}

// macBrowserKind classifies a macOS browser executable by its base name.
func macBrowserKind(path string) string {
	b := lowerBase(path)
	switch {
	case strings.Contains(b, "edge"):
		return "edge"
	case strings.Contains(b, "chrome"):
		return "chrome"
	default:
		return "chromium"
	}
}

// discoverMacBrowser returns the browser to launch, searching in this order:
//
//  1. EnvCobrowseBrowser: an executable path, an .app bundle path, or one of
//     chrome, edge, chromium, canary. A bad value is an error, never a
//     fallback to another browser.
//  2. For each of Chrome, Edge, Chromium, Chrome Canary: /Applications, then
//     ~/Applications (the executable inside the .app bundle).
//  3. google-chrome, chrome, chromium or microsoft-edge on PATH.
//
// On failure the error wraps ErrNoBrowser and lists every location searched.
func discoverMacBrowser(p macBrowserProbe) (BrowserInfo, error) {
	roots := []string{"/Applications"}
	if p.home != "" {
		roots = append(roots, filepath.Join(p.home, "Applications"))
	}
	only := ""
	if override := strings.Trim(strings.TrimSpace(p.getenv(EnvCobrowseBrowser)), `"`); override != "" {
		if p.isExec(override) {
			return BrowserInfo{Path: override, Kind: macBrowserKind(override)}, nil
		}
		if strings.HasSuffix(strings.TrimRight(override, "/"), ".app") {
			bundle := strings.TrimSuffix(filepath.Base(strings.TrimRight(override, "/")), ".app")
			for _, a := range macApps {
				if a.bundle == bundle {
					if bin := macAppBinary(filepath.Dir(strings.TrimRight(override, "/")), a); p.isExec(bin) {
						return BrowserInfo{Path: bin, Kind: a.kind}, nil
					}
				}
			}
			return BrowserInfo{}, fmt.Errorf("%w: %s=%s is an app bundle without a known browser executable inside; use the path to the executable in Contents/MacOS",
				ErrNoBrowser, EnvCobrowseBrowser, override)
		}
		known := false
		for _, a := range macApps {
			if strings.EqualFold(override, a.alias) {
				only, known = a.alias, true
			}
		}
		if !known {
			return BrowserInfo{}, fmt.Errorf("%w: %s=%s is not an existing executable, an .app bundle or a known browser name; use a path to the browser executable, or one of: chrome, edge, chromium, canary",
				ErrNoBrowser, EnvCobrowseBrowser, override)
		}
	}

	var searched []string
	for _, a := range macApps {
		if only != "" && a.alias != only {
			continue
		}
		for _, root := range roots {
			bin := macAppBinary(root, a)
			searched = append(searched, bin)
			if p.isExec(bin) {
				return BrowserInfo{Path: bin, Kind: a.kind}, nil
			}
		}
	}
	if only == "" && p.lookPath != nil {
		for _, name := range macPathNames {
			searched = append(searched, "PATH:"+name)
			if path, err := p.lookPath(name); err == nil && path != "" {
				return BrowserInfo{Path: path, Kind: macBrowserKind(path)}, nil
			}
		}
	}
	if only != "" {
		return BrowserInfo{}, fmt.Errorf("%w: %s=%s found no %s install. Searched: %s",
			ErrNoBrowser, EnvCobrowseBrowser, only, only, strings.Join(searched, "; "))
	}
	return BrowserInfo{}, fmt.Errorf("%w. Install Google Chrome or Microsoft Edge, or set %s to the browser's executable (inside its .app bundle, under Contents/MacOS). Searched: %s",
		ErrNoBrowser, EnvCobrowseBrowser, strings.Join(searched, "; "))
}

// macDefaultBaseDir is the per-user session parent: <home>/Library/Caches/
// Citadel/cobrowse-sessions. With no home dir it returns an error and never
// falls back to a shared temp parent. Its owner-only check is
// ensureCobrowseBaseDir.
func macDefaultBaseDir(home string, homeErr error) (string, error) {
	if homeErr != nil || home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("cannot determine your home folder for the co-browse session directory: %v", homeErr)
	}
	return filepath.Join(home, "Library", "Caches", "Citadel", cobrowseSessionsDirName), nil
}

// macOwnerAliveWithStart reports whether pid is a live process whose start
// token equals start. It errs toward "alive" when it cannot tell (permission
// denied, an unreadable start time, or no recorded start), so a sibling's live
// session is never removed on doubt. sig0 is kill(pid, 0); startOf reads the
// process start token.
func macOwnerAliveWithStart(pid int, start int64, sig0 func(int) error, startOf func(int) (int64, error)) bool {
	if pid <= 0 {
		return false
	}
	if err := sig0(pid); err != nil && errors.Is(err, syscall.ESRCH) {
		return false
	}
	if start == 0 {
		return true
	}
	got, err := startOf(pid)
	if err != nil {
		return true
	}
	return got == start
}

// macCmdlineHasProfile reports whether a browser's command line carries
// --user-data-dir=<dir> for exactly this session dir (not a prefix of a longer
// path).
func macCmdlineHasProfile(cmdline, dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	needle := "--user-data-dir=" + dir
	for i := 0; ; {
		j := strings.Index(cmdline[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end == len(cmdline) || cmdline[end] == ' ' || cmdline[end] == '\n' {
			return true
		}
		i += j + 1
	}
}

// macSweepGrace keeps the sweep away from a session dir that has no pidfile
// yet because a sibling process is still launching into it.
const macSweepGrace = 10 * time.Minute

// macSweepDeps are the process seams of the sweep.
type macSweepDeps struct {
	self      int
	selfStart int64
	// ownerAlive is macOwnerAliveWithStart over the real syscalls.
	ownerAlive func(pid int, start int64) bool
	// cmdline returns a process's full command line.
	cmdline func(pid int) (string, error)
	// pgid returns a process's process group id.
	pgid func(pid int) (int, error)
	// killGroup sends SIGKILL to the whole group; an already-gone group is nil.
	killGroup func(pgid int) error
	// removeDir removes a session dir.
	removeDir func(dir string) error
}

// killVerifiedBrowserGroup kills the process group of an orphaned session's
// browser, and only when the recorded PID is still that browser: its command
// line names this session's exact profile dir, and it leads its own process
// group (the runner starts the browser with Setpgid). Anything else, including
// an unreadable command line, is left alone: fail closed. It reports whether a
// kill was sent.
func killVerifiedBrowserGroup(dir string, browserPID int, d macSweepDeps) bool {
	if browserPID <= 1 || browserPID == d.self {
		return false
	}
	cmdline, err := d.cmdline(browserPID)
	if err != nil || !macCmdlineHasProfile(cmdline, dir) {
		log.Printf("[cobrowse] sweep: pid %d recorded as the browser of %s no longer matches (or is unverifiable); skipping kill", browserPID, dir)
		return false
	}
	pg, err := d.pgid(browserPID)
	if err != nil || pg != browserPID {
		log.Printf("[cobrowse] sweep: pid %d is not the leader of its process group; skipping kill", browserPID)
		return false
	}
	if err := d.killGroup(pg); err != nil {
		log.Printf("[cobrowse] sweep: could not kill browser group %d: %v", pg, err)
		return false
	}
	return true
}

// sweepMacSessions removes session dirs whose owner process is gone, and first
// kills a verified orphan browser group (killVerifiedBrowserGroup). The owner is
// identified by PID plus start time, so a reused PID is not mistaken for it.
// A dir with no pidfile is left for macSweepGrace, then removed. Dirs this
// process re-adopted are skipped. It refuses a base dir that is not owner-only.
func sweepMacSessions(baseDir string, d macSweepDeps) {
	if err := ensureCobrowseBaseDir(baseDir); err != nil {
		log.Printf("[cobrowse] refusing session sweep of %s: %v", baseDir, err)
		return
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() { // Lstat-based: a symlink entry is not a dir and is never followed
			continue
		}
		dir := filepath.Join(baseDir, e.Name())
		if _, keep := sweepProtected.Load(dir); keep {
			continue
		}
		owner, browserPID, _ := readSessionPidfile(dir)
		start := readSessionOwnerStart(dir)
		switch {
		case owner == 0:
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) < macSweepGrace {
				continue
			}
		case owner == d.self && (start == 0 || start == d.selfStart):
			continue // ours; a matching PID with another start is a dead predecessor
		case owner != d.self && d.ownerAlive(owner, start):
			continue
		}
		killVerifiedBrowserGroup(dir, browserPID, d)
		if err := d.removeDir(dir); err != nil {
			log.Printf("[cobrowse] sweep: could not remove stale session dir %s: %v", dir, err)
		}
	}
}

// stopProcessGroup kills the group unless the leader already exited (its
// reaper has then killed the group, and the id may be reused), then waits
// (bounded) for the reaper.
func stopProcessGroup(pgid int, exited <-chan struct{}, kill func(int) error) error {
	select {
	case <-exited:
		return nil
	default:
	}
	err := kill(pgid)
	waitBounded(exited)
	return err
}
