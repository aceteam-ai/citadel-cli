// Package agentsprobe detects vendor coding-agent CLIs (Claude Code, Codex,
// Gemini CLI, OpenCode) already installed and authenticated on this node's
// own PATH, for its own account -- not a container or binary AceTeam ships.
//
// This is DoR-v2 slice S1 of issue #8993 (aceteam-ai/aceteam): "drive
// installed vendor coding agents on user hardware, wrapped in AEP receipts."
// S1 is discovery only -- it never spawns, drives, or runs a turn of any
// vendor agent. Citadel itself makes no network call; the only outbound touch
// is each installed vendor's own `--version`, which some Node CLIs (Gemini,
// sometimes Claude Code) may use to run an update check. Every check here is
// local PATH lookup, a bounded `--version` exec, and a local credential-file
// existence/shape check.
//
// Auth-state honesty: a vendor whose credential-file layout is not
// confidently known is reported as AuthStateUnknown rather than guessed at.
// A wrong guess in the "unauthenticated" direction is a false negative that
// looks like a clean "no"; reporting unknown keeps that distinction visible
// to callers instead of silently asserting something unverified.
package agentsprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// probeTimeout bounds each vendor's --version exec. Mirrors the sibling
// internal/capabilities package's detectionTimeout.
const probeTimeout = 5 * time.Second

// AuthState is a tri-state auth signal. It is deliberately not a bool:
// collapsing "we could not tell" into "no" would misreport an authenticated
// vendor agent as unauthenticated whenever its credential file is merely
// unreadable or has an unexpected shape.
type AuthState string

const (
	// AuthStateAuthed means a credential file was found at the expected
	// path and parsed as a non-empty JSON object. This is a structural
	// signal only -- the token inside is never validated against the
	// vendor's servers (S1 makes no network call), so a stale or revoked
	// token still reads as authed here.
	AuthStateAuthed AuthState = "authed"
	// AuthStateNo means no credential file was found at the expected path.
	AuthStateNo AuthState = "unauthenticated"
	// AuthStateUnknown means auth state could not be determined: the
	// vendor's credential layout is not confidently known, the file
	// exists but could not be read (e.g. permission denied), or it exists
	// but did not parse as a JSON object.
	AuthStateUnknown AuthState = "unknown"
)

// VendorAgent is one probed vendor coding-agent CLI.
type VendorAgent struct {
	// Name is the stable vendor identifier (claude, codex, gemini, opencode).
	Name string `json:"name"`
	// Installed reports whether the binary was found on PATH.
	Installed bool `json:"installed"`
	// Version is the first line of `--version` output, best-effort parsed
	// down to a bare version number when one is found. Empty when the
	// binary is not installed or the version exec failed/timed out.
	Version string `json:"version,omitempty"`
	// Authed is the tri-state auth signal. Empty (omitted) when the
	// binary is not installed -- auth state is not meaningful for an
	// absent binary.
	Authed AuthState `json:"authed,omitempty"`
	// AdapterClass names the driving mechanism a future slice (S4/S5)
	// would use for this vendor: "claude-code-hooks" (headless print mode
	// + PreToolUse/PostToolUse/Stop hooks), "codex-exec-headless", or
	// "zed-acp" (Zed's Agent Client Protocol, JSON-RPC over stdio -- never
	// called bare "ACP" in this codebase; that name is reserved for
	// AceTeam's own Agent Compute Protocol). "unknown" when the mechanism
	// is not yet confidently mapped for this vendor.
	AdapterClass string `json:"adapter_class"`
}

type vendorSpec struct {
	name         string
	binary       string
	adapterClass string
	authCheck    func(homeDir string) AuthState
}

// vendorSpecs is the fixed set of vendors S1 probes for, per the DoR-v2
// discovery slice: "PATH plus auth-state detection per vendor (claude,
// codex, gemini, opencode)."
var vendorSpecs = []vendorSpec{
	{name: "claude", binary: "claude", adapterClass: "claude-code-hooks", authCheck: claudeAuthState},
	{name: "codex", binary: "codex", adapterClass: "codex-exec-headless", authCheck: codexAuthState},
	{name: "gemini", binary: "gemini", adapterClass: "zed-acp", authCheck: geminiAuthState},
	{name: "opencode", binary: "opencode", adapterClass: "unknown", authCheck: opencodeAuthState},
}

// Options controls WHOSE environment Probe inspects. Both fields default (when
// empty) to the current process's own environment -- which is correct for the
// operator command (it runs AS the operator), but systematically wrong inside
// a root citadel-worker.service, where HOME is /root and PATH is a minimal
// systemd set. There a user's ~/.npm-global/bin/claude is invisible and
// /root/.claude is absent, so the process environment yields Installed=false
// and a confident (false) "unauthenticated" for every vendor. S2's worker call
// site fills these in from ResolveTargetUser; S1's `citadel agents probe`
// passes a zero Options so its behavior is byte-unchanged.
type Options struct {
	// HomeDir is the home directory whose per-vendor credential files are
	// checked. Empty means the current process's own home (os.UserHomeDir).
	HomeDir string
	// PathEnv is the PATH list used for binary lookup, in the OS PATH-list
	// format (see filepath.SplitList). Empty means the current process's own
	// PATH, resolved byte-identically via exec.LookPath.
	PathEnv string
}

// Probe detects installed and authenticated vendor coding agents on this node.
// Read-only: it never executes an agent turn. Citadel makes no network call of
// its own; each installed vendor's `--version` may (some Node CLIs run an
// update check on it). Otherwise only PATH lookup, a bounded `--version` exec
// per installed vendor, and a local credential-file presence/shape check.
//
// opts selects whose HOME/PATH is inspected (see Options); a zero Options uses
// the current process's own environment.
func Probe(ctx context.Context, opts Options) []VendorAgent {
	homeDir := opts.HomeDir
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	out := make([]VendorAgent, 0, len(vendorSpecs))
	for _, spec := range vendorSpecs {
		out = append(out, probeVendor(ctx, spec, homeDir, opts.PathEnv))
	}
	return out
}

func probeVendor(ctx context.Context, spec vendorSpec, homeDir, pathEnv string) VendorAgent {
	agent := VendorAgent{Name: spec.name, AdapterClass: spec.adapterClass}

	path, err := lookPath(spec.binary, pathEnv)
	if err != nil {
		return agent // Installed stays false; Authed stays "" (not meaningful).
	}
	agent.Installed = true
	agent.Version = probeVersion(ctx, path)

	if spec.authCheck == nil || homeDir == "" {
		agent.Authed = AuthStateUnknown
		return agent
	}
	agent.Authed = spec.authCheck(homeDir)
	return agent
}

// lookPath resolves binary to an executable path. When pathEnv is empty it
// delegates to exec.LookPath (the current process's PATH), byte-identical to
// the pre-Options behavior the operator command relies on. When pathEnv is a
// non-empty override (S2 supplies the target user's PATH), it walks that list
// instead, since exec.LookPath can only read the process's own PATH env.
//
// The override walk uses POSIX semantics (an executable bit on a regular
// file). Windows PATHEXT resolution is deferred, consistent with this
// package's other POSIX-only paths -- S2 targets Linux/macOS worker nodes.
func lookPath(binary, pathEnv string) (string, error) {
	if pathEnv == "" {
		return exec.LookPath(binary)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue // An empty PATH entry means "current dir"; skip it.
		}
		candidate := filepath.Join(dir, binary)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", exec.ErrNotFound
}

// ResolveTargetUser resolves the HOME/PATH of the node's OWNING user for a
// caller (S2's `citadel work`) that may be running as root/systemd rather than
// as that user. It mirrors platform.resolveConfigDir's SUDO_USER handling:
// prefer the invoking non-root user under sudo, else fall back to the
// process's own home. It is ready for S2's call site; S1 does not use it.
//
// LIMIT (state it, don't paper over it): a shipped citadel-worker.service is
// NOT launched via sudo, so SUDO_USER is unset there and this falls back to
// the process home = /root -- the SAME wrong answer the #1005 motivation
// describes. This helper fixes `sudo citadel work`; the bare-systemd fleet
// case needs a different owner signal (the unit's User=, or the node
// config-dir owner uid), which S2's design review must choose. An
// unresolvable target user is returned as an error so the caller can pass a
// zero HomeDir and Probe reports AuthStateUnknown rather than a confident
// false "no".
func ResolveTargetUser() (Options, error) {
	return resolveTargetUser(os.Getenv("SUDO_USER"), lookupUserHome, os.UserHomeDir, os.Getenv("PATH"))
}

// lookupUserHome is the real user-home resolver behind ResolveTargetUser,
// split out so resolveTargetUser is unit-testable without real accounts.
func lookupUserHome(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// resolveTargetUser is the testable core of ResolveTargetUser. Dependencies
// are injected so the SUDO_USER/passwd/fallback branches are exercisable
// without privileges or real users.
func resolveTargetUser(
	sudoUser string,
	lookupHome func(string) (string, error),
	processHome func() (string, error),
	processPath string,
) (Options, error) {
	// Prefer the invoking user under sudo/systemd-set SUDO_USER, exactly as
	// platform.resolveConfigDir does -- including treating "root" as unset.
	if sudoUser != "" && sudoUser != "root" {
		home, err := lookupHome(sudoUser)
		if err != nil {
			return Options{}, fmt.Errorf("resolve home for SUDO_USER %q: %w", sudoUser, err)
		}
		return Options{HomeDir: home, PathEnv: userPathEnv(home, processPath)}, nil
	}
	home, err := processHome()
	if err != nil {
		return Options{}, fmt.Errorf("resolve process home directory: %w", err)
	}
	return Options{HomeDir: home, PathEnv: userPathEnv(home, processPath)}, nil
}

// userPathEnv builds a PATH that prepends the common user-local bin dirs where
// Node/pip/cargo-installed vendor CLIs land (~/.npm-global/bin the #1005
// motivating case) to whatever PATH the process already had. Known gap: it
// does NOT reconstruct nvm/volta/bun/asdf version-specific bin dirs, which
// live under unpredictable per-version subpaths and would need reading the
// user's shell profile.
func userPathEnv(home, processPath string) string {
	userBins := []string{
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
	}
	if processPath == "" {
		return strings.Join(userBins, string(os.PathListSeparator))
	}
	return strings.Join(append(userBins, processPath), string(os.PathListSeparator))
}

// probeVersion runs `<path> --version` under probeTimeout and best-effort
// parses a version number out of the first line of output. Returns "" on
// any exec failure or timeout -- a missing version is not itself an error
// here, just an absent field.
func probeVersion(ctx context.Context, path string) string {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, "--version")
	// A daemonizing `--version` (or one that spawns a lingering child) can
	// keep the stdout pipe open past ctx cancellation, wedging the .Output()
	// copy goroutine indefinitely. WaitDelay bounds how long Wait blocks on
	// that goroutine after the context fires, so a misbehaving vendor binary
	// cannot stall the probe -- the classic exec.CommandContext pitfall.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseVersion(string(out))
}

var versionNumberPattern = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// parseVersion extracts a bare version number (e.g. "1.2.3") from raw
// `--version` output, falling back to the trimmed first line when no
// version-shaped substring is found.
func parseVersion(raw string) string {
	firstLine := strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	if m := versionNumberPattern.FindString(firstLine); m != "" {
		return m
	}
	return firstLine
}

// jsonFileNonEmptyObjectState reports the auth-state signal implied by a
// vendor credential file's presence and shallow structural validity. It
// never reads, logs, or returns any field VALUE from the file -- only
// whether it exists and decodes into a JSON object with at least one key.
// This is a structural signal, not a validity check: an expired or revoked
// token still reads as AuthStateAuthed (S1 makes no network call to verify
// tokens against the vendor).
func jsonFileNonEmptyObjectState(path string) AuthState {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AuthStateNo
		}
		// Permission denied or another read error: ambiguous, never a
		// false "no".
		return AuthStateUnknown
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return AuthStateUnknown
	}
	if len(obj) == 0 {
		return AuthStateNo
	}
	return AuthStateAuthed
}

// credentialFileState wraps jsonFileNonEmptyObjectState with relocation
// awareness. jsonFileNonEmptyObjectState reports a CONFIDENT AuthStateNo when
// the default credential file is absent (or present but an empty object) --
// but that confidence is only warranted when the credentials could not live
// anywhere else. They often can: a vendor may relocate its config dir via an
// env var (CLAUDE_CONFIG_DIR, CODEX_HOME), or store creds in the macOS
// keychain / a different Windows layout entirely. So when the default check
// says "no" AND either a known relocationVar is set OR goos != "linux", this
// degrades to AuthStateUnknown, honoring the package's auth-state honesty
// principle (a confident false "no" is worse than an honest "unknown").
//
// goos is passed in rather than read from runtime.GOOS directly so the
// non-linux branch is table-testable on a linux CI runner (the
// resolveConfigDir injectable-core pattern). LIMIT: relocationVars are read
// from the CURRENT PROCESS environment, which under a root worker is not the
// target user's shell environment -- so a user-set CLAUDE_CONFIG_DIR the
// worker never inherited will not be observed here.
func credentialFileState(path string, relocationVars []string, goos string) AuthState {
	state := jsonFileNonEmptyObjectState(path)
	if state != AuthStateNo {
		return state // Authed, or an ambiguous Unknown -- both already correct.
	}
	if goos != "linux" {
		return AuthStateUnknown
	}
	for _, v := range relocationVars {
		if os.Getenv(v) != "" {
			return AuthStateUnknown
		}
	}
	return AuthStateNo
}

// claudeAuthState checks Claude Code's credential file. Claude Code also
// supports relocating its config dir via CLAUDE_CONFIG_DIR, and on macOS may
// store credentials in the system keychain instead -- neither of which this
// file-based check can see. credentialFileState degrades an absent default
// file to AuthStateUnknown (never a confident AuthStateNo) whenever
// CLAUDE_CONFIG_DIR is set or the OS is non-linux.
func claudeAuthState(homeDir string) AuthState {
	return credentialFileState(
		filepath.Join(homeDir, ".claude", ".credentials.json"),
		[]string{"CLAUDE_CONFIG_DIR"},
		runtime.GOOS,
	)
}

// codexAuthState checks the Codex CLI's credential file. Codex supports
// relocating its home via CODEX_HOME; credentialFileState degrades an absent
// default file to AuthStateUnknown when CODEX_HOME is set or the OS is
// non-linux.
func codexAuthState(homeDir string) AuthState {
	return credentialFileState(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]string{"CODEX_HOME"},
		runtime.GOOS,
	)
}

// geminiAuthState checks the Gemini CLI's OAuth credential file. No
// credential-relocation env var is confidently known for the Gemini CLI, so
// none is enumerated (inventing one would be a confidently-wrong guess, the
// exact failure mode this package's honesty principle exists to avoid).
// credentialFileState still degrades an absent default file to
// AuthStateUnknown on non-linux, where creds may live outside this file.
func geminiAuthState(homeDir string) AuthState {
	return credentialFileState(
		filepath.Join(homeDir, ".gemini", "oauth_creds.json"),
		nil,
		runtime.GOOS,
	)
}

// opencodeAuthState always reports AuthStateUnknown: OpenCode's credential
// file layout is not yet confidently known to this package. Guessing a path
// here risks a false AuthStateNo for a node that is, in fact, authenticated
// through a layout this function doesn't check -- see the package doc's
// auth-state honesty note.
func opencodeAuthState(string) AuthState {
	return AuthStateUnknown
}
