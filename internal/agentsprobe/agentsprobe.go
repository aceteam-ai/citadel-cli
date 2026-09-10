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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// credentialFileReadLimit caps how many bytes are read from a vendor credential
// file. A real credentials.json is tiny; the cap exists so a target-controlled
// file cannot make a (possibly root) worker read an unbounded amount.
const credentialFileReadLimit = 1 << 20 // 1 MiB

// versionOutputLimit caps stdout captured from a `--version` exec. A misbehaving
// (or hostile) target-uid binary that emits gigabytes must not OOM the worker;
// parseVersion only needs the first line.
const versionOutputLimit = 64 * 1024

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
	// checked. Empty means the current process's own home (os.UserHomeDir) --
	// UNLESS HomeUnknown is set (see below), which distinguishes "use the
	// process home" from "the target could not be resolved".
	HomeDir string
	// PathEnv is the PATH list used for binary lookup, in the OS PATH-list
	// format (see filepath.SplitList). Empty means the current process's own
	// PATH, resolved byte-identically via exec.LookPath.
	PathEnv string
	// HomeUnknown means the target user could NOT be resolved: do not substitute
	// the worker's own home, and report AuthStateUnknown for every installed
	// vendor rather than a confident false "no" against /root (citadel #1015).
	// This is the honest degradation the S2 worker uses when ResolveTargetUserForNode
	// fails; it is distinct from HomeDir=="" which means "probe the process's own
	// home" (the S1 operator command's zero Options).
	HomeUnknown bool
	// DropTo, when non-nil, runs each vendor's `--version` exec as this
	// credential with a minimal env (privilege drop). It is set ONLY by the S2
	// worker path when the worker is root and the target is a different,
	// non-root account -- exec'ing a target-user-writable binary (its PATH
	// starts with ~/.npm-global/bin) as root would be a local privilege
	// escalation. Nil for the operator command and for a non-root worker (which
	// has no privilege to drop). See Target.probeOptions.
	DropTo *Credential
}

// Credential is the uid/gid (and username, for the child env) the `--version`
// exec drops to under a root worker. Cross-platform type; the drop itself is
// applied only on POSIX by applyExecHardening (a no-op stub on Windows).
type Credential struct {
	UID      int
	GID      int
	Username string
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
	// Substitute the process's own home ONLY for the operator command's zero
	// Options (HomeDir=="" && !HomeUnknown). When HomeUnknown is set the target
	// was unresolvable, and probing the worker's /root would produce the exact
	// confident-false "no" the honesty principle exists to prevent (#1015).
	if homeDir == "" && !opts.HomeUnknown {
		homeDir, _ = os.UserHomeDir()
	}
	out := make([]VendorAgent, 0, len(vendorSpecs))
	for _, spec := range vendorSpecs {
		out = append(out, probeVendor(ctx, spec, homeDir, opts))
	}
	return out
}

func probeVendor(ctx context.Context, spec vendorSpec, homeDir string, opts Options) VendorAgent {
	agent := VendorAgent{Name: spec.name, AdapterClass: spec.adapterClass}

	path, err := lookPath(spec.binary, opts.PathEnv)
	if err != nil {
		return agent // Installed stays false; Authed stays "" (not meaningful).
	}
	agent.Installed = true
	agent.Version = probeVersion(ctx, path, opts)

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

// ResolveTargetUser resolves the HOME/PATH of the node's OWNING user via
// SUDO_USER, falling back to the process's own home. SUPERSEDED for the S2
// worker by ResolveTargetUserForNode (resolver.go), which adds the config
// override and node-dir-owner tiers this helper lacks -- exactly the
// bare-systemd fleet case the LIMIT below names. Kept for its tests and as the
// tier-3/4 reference; no non-test caller.
//
// LIMIT: a shipped citadel-worker.service is NOT launched via sudo, so
// SUDO_USER is unset there and this falls back to the process home = /root --
// the wrong answer the #1005 motivation describes, which is why the S2 worker
// uses ResolveTargetUserForNode instead.
//
// On an unresolvable target the S2 worker must pass Options{HomeUnknown: true}
// (NOT a zero HomeDir, which means "use the process home") so Probe reports
// AuthStateUnknown rather than a confident false "no" against /root -- the
// #1015 fix, since a zero HomeDir alone does not do that.
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
// motivating case) to whatever PATH the process already had.
//
// nvm installs Node under a per-version subpath, so a read-only GLOB recovers
// those bin dirs (a directory listing, never shell-profile execution -- see the
// S2 design 2c: sourcing the user's profile would run their shell with the
// worker's privileges). Known gap after this: an install location none of these
// static entries cover still reports Installed=false, a documented confident
// false, not fixed here.
func userPathEnv(home, processPath string) string {
	userBins := []string{
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".local", "share", "pnpm"),
	}
	// Read-only glob for nvm's per-version node bin dirs. filepath.Glob only
	// lists directories; it never executes anything.
	if matches, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin")); err == nil {
		userBins = append(userBins, matches...)
	}
	// Common system install locations (Homebrew, manual /usr/local), appended
	// only when the process PATH doesn't already carry them.
	for _, sys := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !pathListContains(processPath, sys) {
			userBins = append(userBins, sys)
		}
	}
	if processPath == "" {
		return strings.Join(userBins, string(os.PathListSeparator))
	}
	return strings.Join(append(userBins, processPath), string(os.PathListSeparator))
}

// pathListContains reports whether dir is an exact entry of the OS PATH list.
func pathListContains(pathEnv, dir string) bool {
	for _, d := range filepath.SplitList(pathEnv) {
		if d == dir {
			return true
		}
	}
	return false
}

// probeVersion runs `<path> --version` under probeTimeout and best-effort
// parses a version number out of the first line of output. Returns "" on any
// exec failure or timeout -- a missing version is not itself an error here,
// just an absent field. The exec is hardened (privilege drop, minimal env,
// bounded output, process-group reaping) by buildVersionCmd; see there.
func probeVersion(ctx context.Context, path string, opts Options) string {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := buildVersionCmd(cctx, path, opts)
	out := &limitedBuffer{limit: versionOutputLimit}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return ""
	}
	return parseVersion(out.String())
}

// buildVersionCmd constructs the `<path> --version` command with every safety
// property S2 requires. Split out so a test can inspect SysProcAttr (the
// privilege-drop assertion) without executing anything.
func buildVersionCmd(ctx context.Context, path string, opts Options) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, "--version")
	// WaitDelay bounds how long Wait blocks on a lingering stdout pipe after ctx
	// fires (a daemonizing `--version`) -- the classic exec.CommandContext pitfall.
	cmd.WaitDelay = time.Second
	if opts.DropTo != nil {
		// Privilege drop: run as the target user with a MINIMAL env, not the
		// worker's. HOME and cwd point at the target's home so a `--version`
		// that side-writes (~/.claude, update-check caches) leaves TARGET-owned
		// files there -- run as root with HOME=/root it pollutes root's home;
		// run as root with HOME=<user> it leaves ROOT-owned droppings that break
		// the user's own CLI later with EACCES. Both are the landmine the drop closes.
		cmd.Env = minimalChildEnv(opts.HomeDir, opts.PathEnv, opts.DropTo.Username)
		if opts.HomeDir != "" {
			// Without this, a unit with WorkingDirectory=/root gives the
			// target-uid child an inaccessible cwd -> Node dies on uv_cwd EACCES
			// -> a false empty version.
			cmd.Dir = opts.HomeDir
		}
	}
	// Setpgid + a process-group Cancel so a `--version` that forks a daemonizing
	// grandchild (an update-check helper) is reaped with the parent on timeout;
	// Credential (when DropTo != nil) drops privileges. Applied by the
	// platform-specific helper -- a no-op on Windows.
	applyExecHardening(cmd, opts.DropTo)
	return cmd
}

// minimalChildEnv is the environment handed to a dropped-privilege `--version`
// exec: identity + PATH only, deliberately NOT the worker's env. A couple of
// safe, non-identity passthroughs are included so a CLI that needs a locale can
// still run.
func minimalChildEnv(home, pathEnv, username string) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + pathEnv,
	}
	if username != "" {
		env = append(env, "USER="+username, "LOGNAME="+username)
	}
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}
	return env
}

// limitedBuffer is a bytes.Buffer that stops accepting data past limit, so a
// misbehaving `--version` cannot OOM the (possibly root) worker. Writes past the
// cap are silently discarded but always reported as fully written, so the child
// never sees a short-write error.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

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
	// SECURITY: this is the ONE place the worker's (possibly root) privilege
	// touches the target user's files. It reads ONLY whether the file exists and
	// decodes as a non-empty JSON OBJECT -- never a field VALUE. The S1 contract
	// forbids credential values reaching the output; a future edit that logs the
	// parse error with a snippet, or returns any value read here, breaks that
	// contract and MUST NOT be added.
	//
	// Lstat + IsRegular guards against a target-controlled non-regular file at
	// this path: a root ReadFile on a FIFO/device blocks forever, and because the
	// probe runs inside the service singleflight lock that would wedge every
	// future refresh for the process lifetime (a local DoS). A symlink is rejected
	// too (never follow one as root). Residual stat->open TOCTOU: the target could
	// swap the regular file for a FIFO between the two calls; it is not chased with
	// O_NONBLOCK because the read is ALSO size-bounded, so the worst case is a
	// bounded read, not an unbounded block.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AuthStateNo
		}
		// Permission denied or another stat error: ambiguous, never a false "no".
		return AuthStateUnknown
	}
	if !info.Mode().IsRegular() {
		return AuthStateUnknown
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AuthStateNo
		}
		return AuthStateUnknown
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, credentialFileReadLimit))
	if err != nil {
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
