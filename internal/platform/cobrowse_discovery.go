// internal/platform/cobrowse_discovery.go
//
// Windows browser discovery (aceteam#13503 design section 3.3). The decision is a
// pure function over injected probes so its order is unit-testable on any host;
// cobrowse_runner_windows.go supplies the real environment, file system and
// registry probes.
package platform

import (
	"fmt"
	"strings"
)

// windowsBrowserProbe is the injected view of a Windows machine discovery reads.
type windowsBrowserProbe struct {
	// getenv reads an environment variable.
	getenv func(string) string
	// isFile reports whether path is an existing regular file.
	isFile func(string) bool
	// appPath returns the default value of
	// <hive>\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\<exe>, or "".
	// hive is "HKCU" or "HKLM".
	appPath func(hive, exe string) string
	// lookPath resolves a command on PATH.
	lookPath func(string) (string, error)
}

// winJoin joins Windows path elements with a backslash regardless of the host
// OS, so the candidate list is identical in tests on Linux and on Windows.
func winJoin(parts ...string) string {
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if i > 0 {
			p = strings.Trim(p, `\/`)
		} else {
			p = strings.TrimRight(p, `\/`)
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, `\`)
}

// discoverWindowsBrowser returns the browser to launch, searching in this order:
//
//  1. EnvCobrowseBrowser (explicit; a missing file is an error, never a fallback)
//  2. per-user Chrome under %LOCALAPPDATA%
//  3. machine Chrome under %ProgramFiles% and %ProgramFiles(x86)%
//  4. App Paths registry entries for chrome.exe then msedge.exe, HKCU before HKLM
//  5. Edge under %ProgramFiles(x86)% and %ProgramFiles%
//  6. chrome.exe / msedge.exe on PATH
//
// On failure the error wraps ErrNoBrowser and lists every location searched.
//
// EnvCobrowseBrowser may also be a browser name ("chrome" or "edge", any case).
// A name runs the same search restricted to that kind, in the same order; it
// never falls back to the other browser.
func discoverWindowsBrowser(p windowsBrowserProbe) (BrowserInfo, error) {
	kind := ""
	if override := strings.TrimSpace(p.getenv(EnvCobrowseBrowser)); override != "" {
		override = strings.Trim(override, `"`)
		if p.isFile(override) {
			return BrowserInfo{Path: override, Kind: browserKindForPath(override)}, nil
		}
		name := strings.ToLower(override)
		if name != "chrome" && name != "edge" {
			return BrowserInfo{}, fmt.Errorf("%w: %s=%s is not an existing file or a known browser name; use a path to the browser executable, or one of: chrome, edge",
				ErrNoBrowser, EnvCobrowseBrowser, override)
		}
		kind = name
	}
	wantChrome := kind == "" || kind == "chrome"
	wantEdge := kind == "" || kind == "edge"

	var searched []string
	try := func(path string) (BrowserInfo, bool) {
		if path == "" {
			return BrowserInfo{}, false
		}
		searched = append(searched, path)
		if p.isFile(path) {
			return BrowserInfo{Path: path, Kind: browserKindForPath(path)}, true
		}
		return BrowserInfo{}, false
	}
	under := func(envVar string, rest ...string) string {
		root := strings.TrimSpace(p.getenv(envVar))
		if root == "" {
			return ""
		}
		return winJoin(append([]string{root}, rest...)...)
	}

	chromeRel := []string{"Google", "Chrome", "Application", "chrome.exe"}
	edgeRel := []string{"Microsoft", "Edge", "Application", "msedge.exe"}

	if wantChrome {
		for _, c := range []string{
			under("LOCALAPPDATA", chromeRel...),
			under("ProgramFiles", chromeRel...),
			under("ProgramFiles(x86)", chromeRel...),
		} {
			if info, ok := try(c); ok {
				return info, nil
			}
		}
	}

	if p.appPath != nil {
		for _, exe := range filterExes(wantChrome, wantEdge) {
			for _, hive := range []string{"HKCU", "HKLM"} {
				v := strings.Trim(strings.TrimSpace(p.appPath(hive, exe)), `"`)
				if v == "" {
					searched = append(searched, fmt.Sprintf(`%s App Paths\%s (absent)`, hive, exe))
					continue
				}
				if info, ok := try(v); ok {
					return info, nil
				}
			}
		}
	}

	if wantEdge {
		for _, c := range []string{
			under("ProgramFiles(x86)", edgeRel...),
			under("ProgramFiles", edgeRel...),
		} {
			if info, ok := try(c); ok {
				return info, nil
			}
		}
	}

	if p.lookPath != nil {
		for _, name := range filterExes(wantChrome, wantEdge) {
			searched = append(searched, "PATH:"+name)
			if path, err := p.lookPath(name); err == nil && path != "" {
				return BrowserInfo{Path: path, Kind: browserKindForPath(path)}, nil
			}
		}
	}

	if kind != "" {
		return BrowserInfo{}, fmt.Errorf("%w: %s=%s found no %s install. Searched: %s",
			ErrNoBrowser, EnvCobrowseBrowser, kind, kind, strings.Join(searched, "; "))
	}
	return BrowserInfo{}, fmt.Errorf("%w. Install Microsoft Edge or Google Chrome, or set %s to the browser's .exe. Searched: %s",
		ErrNoBrowser, EnvCobrowseBrowser, strings.Join(searched, "; "))
}

// filterExes returns the App Paths / PATH executable names for the wanted
// browser kinds, Chrome first.
func filterExes(chrome, edge bool) []string {
	var out []string
	if chrome {
		out = append(out, "chrome.exe")
	}
	if edge {
		out = append(out, "msedge.exe")
	}
	return out
}
