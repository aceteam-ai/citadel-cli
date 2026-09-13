// internal/service/launchd_plist.go
//
// Pure, platform-independent launchd helpers: plist rendering, ProgramArguments
// parsing/rewriting, launchctl domain-target and bootstrap/bootout argv
// construction. Kept out of the //go:build darwin launchd.go so the logic
// (including the RunAtLoad/KeepAlive reboot-survival keys) is unit-testable on
// the Linux CI host -- a darwin-tagged test never runs there (citadel-cli#1043).
package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// launchdLabel is the launchd job label (and plist basename) for the citadel
// node service. Shared by the darwin Manager (launchd.go) and the pure helpers
// here.
const launchdLabel = "ai.aceteam.citadel"

// launchdPlistInput is the fully-resolved set of values needed to render a
// launchd plist, with no I/O -- the darwin Manager resolves the real home/log
// dirs and calls renderLaunchdPlist, so rendering stays pure.
type launchdPlistInput struct {
	Label     string
	ExecPath  string
	Args      []string
	HomeDir   string
	LogDir    string
	RunAtLoad bool
	KeepAlive bool
}

// xmlEscape escapes a value for inclusion in a plist <string>. A home dir or
// argument containing & < > " ' would otherwise produce a malformed plist that
// launchd silently refuses to load.
func xmlEscape(s string) string {
	var b strings.Builder
	// xml.EscapeText only errors on an unwritable writer; strings.Builder never
	// fails, so the error is safe to drop.
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func plistBool(v bool) string {
	if v {
		return "<true/>"
	}
	return "<false/>"
}

// renderLaunchdPlist produces the launchd plist XML for the given input. It
// always emits RunAtLoad and KeepAlive keys (their values come from the input,
// but the citadel service sets both true) so the service starts at load and is
// relaunched if it exits -- the reboot/login survival contract.
func renderLaunchdPlist(in launchdPlistInput) string {
	var progArgs strings.Builder
	progArgs.WriteString(fmt.Sprintf("        <string>%s</string>\n", xmlEscape(in.ExecPath)))
	for _, a := range in.Args {
		progArgs.WriteString(fmt.Sprintf("        <string>%s</string>\n", xmlEscape(a)))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    %s
    <key>KeepAlive</key>
    %s
    <key>StandardOutPath</key>
    <string>%s/citadel.log</string>
    <key>StandardErrorPath</key>
    <string>%s/citadel-error.log</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
        <key>CITADEL_SERVICE</key>
        <string>true</string>
    </dict>
</dict>
</plist>
`, xmlEscape(in.Label), progArgs.String(),
		plistBool(in.RunAtLoad), plistBool(in.KeepAlive),
		xmlEscape(in.LogDir), xmlEscape(in.LogDir), xmlEscape(in.HomeDir), xmlEscape(in.HomeDir))
}

// launchdDomainTarget returns the launchctl domain target for a service:
//   - system daemon (userMode=false): "system"
//   - user agent   (userMode=true):   "gui/<uid>"
//
// The uid is ignored for the system domain. Modern launchctl (bootstrap/bootout/
// kickstart) requires an explicit domain target, unlike the legacy load/unload
// verbs which inferred it from the plist location and current session.
func launchdDomainTarget(userMode bool, uid int) string {
	if userMode {
		return fmt.Sprintf("gui/%d", uid)
	}
	return "system"
}

// bootstrapArgs builds the argv for `launchctl bootstrap <domain> <plist>`, the
// modern replacement for `launchctl load`.
func bootstrapArgs(domainTarget, plistPath string) []string {
	return []string{"bootstrap", domainTarget, plistPath}
}

// bootoutArgs builds the argv for `launchctl bootout <domain> <plist>`, the
// modern replacement for `launchctl unload`.
func bootoutArgs(domainTarget, plistPath string) []string {
	return []string{"bootout", domainTarget, plistPath}
}

// resolveLaunchUID returns the uid to use for a user (gui/<uid>) domain target.
// It prefers SUDO_UID so `sudo citadel ...` still targets the invoking user's
// GUI session rather than root's non-existent gui/0, falling back to the
// process uid.
func resolveLaunchUID() int {
	if s := os.Getenv("SUDO_UID"); s != "" {
		if uid, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return uid
		}
	}
	return os.Getuid()
}

// isCitadelManagedPlist reports whether a plist was written by citadel and is
// therefore safe to rewrite (mirrors isCitadelManagedUnit for systemd). It
// requires the citadel launchd label AND a ProgramArguments whose executable is
// citadel invoked with the `work` subcommand.
func isCitadelManagedPlist(content string) bool {
	if !strings.Contains(content, launchdLabel) {
		return false
	}
	args := parseLaunchdProgramArguments(content)
	if len(args) == 0 || !strings.Contains(args[0], "citadel") {
		return false
	}
	for _, a := range args[1:] {
		if a == "work" {
			return true
		}
	}
	return false
}

// parseLaunchdProgramArguments extracts the ProgramArguments <string> values
// from a plist, in order (element 0 is the ExecPath, the rest are args). It is a
// deliberately small, dependency-free scanner (not a full plist parser) matching
// the exact shape renderLaunchdPlist emits. Returns nil when ProgramArguments is
// absent or malformed.
func parseLaunchdProgramArguments(content string) []string {
	idx := strings.Index(content, "<key>ProgramArguments</key>")
	if idx < 0 {
		return nil
	}
	rest := content[idx:]
	arrStart := strings.Index(rest, "<array>")
	arrEnd := strings.Index(rest, "</array>")
	if arrStart < 0 || arrEnd < 0 || arrEnd < arrStart {
		return nil
	}
	block := rest[arrStart+len("<array>") : arrEnd]
	var out []string
	for {
		s := strings.Index(block, "<string>")
		if s < 0 {
			break
		}
		e := strings.Index(block[s:], "</string>")
		if e < 0 {
			break
		}
		val := block[s+len("<string>") : s+e]
		out = append(out, xmlUnescape(val))
		block = block[s+e+len("</string>"):]
	}
	return out
}

// replaceFirstProgramArgument replaces the first ProgramArguments <string>
// (the ExecPath) with newExec (XML-escaped). It is pure and returns whether it
// changed anything -- used to heal a launchd plist whose baked ExecPath drifted
// (e.g. an old binary baked a versioned Homebrew Cellar path that `brew upgrade`
// later removes). Everything else in the plist is preserved verbatim.
func replaceFirstProgramArgument(content, newExec string) (string, bool) {
	idx := strings.Index(content, "<key>ProgramArguments</key>")
	if idx < 0 {
		return content, false
	}
	arrRel := strings.Index(content[idx:], "<array>")
	if arrRel < 0 {
		return content, false
	}
	arrPos := idx + arrRel
	arrEndRel := strings.Index(content[arrPos:], "</array>")
	sRel := strings.Index(content[arrPos:], "<string>")
	if sRel < 0 {
		return content, false
	}
	sPos := arrPos + sRel
	// The <string> must fall inside this array, not a later one.
	if arrEndRel >= 0 && sRel > arrEndRel {
		return content, false
	}
	eRel := strings.Index(content[sPos:], "</string>")
	if eRel < 0 {
		return content, false
	}
	ePos := sPos + eRel
	updated := content[:sPos] + "<string>" + xmlEscape(newExec) + "</string>" + content[ePos+len("</string>"):]
	return updated, updated != content
}

// xmlUnescape reverses xmlEscape for the small entity set it produces, so a
// parsed ExecPath round-trips. Paths rarely contain XML entities; this keeps
// the Cellar-detection and comparison correct if they do.
func xmlUnescape(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&apos;", "'",
		"&#39;", "'",
		"&#34;", `"`,
	)
	return r.Replace(s)
}
