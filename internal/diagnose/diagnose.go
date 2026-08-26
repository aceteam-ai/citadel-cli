// Package diagnose implements the pure, injectable core behind
// `citadel service diagnose <name>` (citadel #852): one command that explains
// why a managed service is down or unhealthy, instead of the multi-step
// manual dance diagnosing a stuck citadel-vllm took on 2026-08-25 (3x
// `docker logs` + grepping the compose command + a manual `nvidia-smi`, to
// find three separate root causes: a missing --trust-remote-code, an
// embedding model served with a chat command, and ~6.3GB free VRAM against a
// ~16GB need).
//
// Diagnose is pure and side-effect-free by construction: it takes an already
// -gathered Input (the CLI layer in cmd/service_diagnose.go owns every
// docker/compose/filesystem call) and returns a Report. That split is what
// makes every check here unit-testable without docker, and it is also what
// keeps this package honest about degrading: a zero-value field (no
// container, no log, no GPU signal, no declared need) makes the affected
// check report "unknown" rather than panicking or guessing.
package diagnose

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ContainerState is the docker-level state of the service's container. The
// zero value (Exists=false) means no container could be found/inspected --
// normal for a service that was never started, not an error.
type ContainerState struct {
	Exists   bool   `json:"exists"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status,omitempty"` // docker's raw State.Status: running, exited, restarting, ...
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"` // docker's own State.Error (OCI runtime errors etc.)
}

// Verdicts for EnvCheck.
const (
	EnvOK      = "ok"
	EnvMissing = "missing"
	EnvEmpty   = "empty"
)

// EnvCheck is the resolved verdict for one ${VAR...} token referenced
// anywhere in the service's compose file (ports, command, environment, ...).
type EnvCheck struct {
	Var      string `json:"var"`
	Value    string `json:"value,omitempty"` // resolved value; redacted if it looks like a secret
	Required bool   `json:"required"`        // compose guards it with the ${VAR:?...} form
	Default  string `json:"default,omitempty"`
	Verdict  string `json:"verdict"`
}

// Verdicts for VRAMFit.
const (
	VRAMFits         = "fits"
	VRAMInsufficient = "insufficient"
	VRAMUnknown      = "unknown"
)

// VRAMFit is the verdict for whether the service's declared VRAM need fits
// the node's currently free VRAM (citadel #833's memory.free signal).
type VRAMFit struct {
	FreeMB   int    `json:"free_mb"`
	NeedMB   int    `json:"need_mb,omitempty"`
	HaveFree bool   `json:"have_free"` // false when no GPU reported a memory total
	HaveNeed bool   `json:"have_need"` // false when the caller supplied no need estimate
	Verdict  string `json:"verdict"`
}

// Input is everything the diagnose core needs, gathered by the CLI layer.
// Every field degrades independently: a zero-value Input still produces a
// (mostly "unknown") Report rather than an error.
type Input struct {
	ServiceName string
	// Managed is true when the service is declared in citadel.yaml's
	// services: list (a docker-compose-backed managed service).
	Managed bool
	// ComposeRaw is the raw compose YAML for this service's file, used to
	// extract the effective command and every ${VAR...} it references. Empty
	// when the compose file is unknown/unreadable -- the compose-derived
	// checks are then simply omitted.
	ComposeRaw string
	Container  ContainerState
	// LogTail is a bounded tail of the container's combined stdout/stderr.
	LogTail string
	// ResolvedEnv is the env citadel would inject for this compose
	// invocation (host ports, workspace, ...) -- what the compose file's
	// ${VAR...} tokens actually resolve against on this node.
	ResolvedEnv map[string]string
	// EnvFileKeys names the subset of ResolvedEnv keys whose value came from
	// the install-time sibling <name>.env (internal/compose/envfile.go),
	// which is a documented secret-bearing file (catalog installs write API
	// keys there, 0600). Every one of these is redacted unconditionally in
	// the EnvChecks/ComposeCommand sections of the report -- name-pattern
	// matching (secretVarRe) alone is not a safe default for a file whose
	// whole purpose is to carry config a compose ${VAR:?...} guard needs,
	// with no naming convention citadel controls. NOT consulted for the raw
	// log-tail scrub (redactSecretsFromLog uses secretVarRe only there) --
	// see that function's doc comment for why.
	EnvFileKeys  map[string]bool
	FreeVRAMMB   int
	HaveFreeVRAM bool
	NeedVRAMMB   int
	HaveNeedVRAM bool
}

// Report is the rendered diagnosis.
type Report struct {
	ServiceName     string         `json:"service"`
	Managed         bool           `json:"managed"`
	Container       ContainerState `json:"container"`
	RootError       string         `json:"root_error,omitempty"`
	LogTail         string         `json:"log_tail,omitempty"`
	ComposeCommand  string         `json:"compose_command,omitempty"`
	EnvChecks       []EnvCheck     `json:"env_checks,omitempty"`
	VRAM            VRAMFit        `json:"vram"`
	Hints           []string       `json:"hints,omitempty"`
	MostLikelyCause string         `json:"most_likely_cause"`
	SuggestedAction string         `json:"suggested_action"`
}

// maxLogTailBytes bounds how much log text rides in the report (JSON payload
// and terminal output alike). The most recent bytes are kept -- they are
// closest to whatever just failed.
const maxLogTailBytes = 8000

// Diagnose is the pure entry point: gather in the CLI layer, decide here.
func Diagnose(in Input) Report {
	// Scrub secret-looking resolved env values out of the log tail BEFORE
	// anything else reads it (root-error extraction, hint detection, the
	// stored LogTail itself). Without this, a value redacted everywhere else
	// in the report (compose command, env checks) could still leak back out
	// verbatim if an engine happens to echo its own argv/config to stdout.
	logTail := redactSecretsFromLog(in.LogTail, in.ResolvedEnv)

	r := Report{
		ServiceName: in.ServiceName,
		Managed:     in.Managed,
		Container:   in.Container,
		LogTail:     boundLogTail(logTail),
		RootError:   ExtractRootError(logTail),
		Hints:       DetectHints(logTail),
		VRAM:        CheckVRAMFit(in.FreeVRAMMB, in.HaveFreeVRAM, in.NeedVRAMMB, in.HaveNeedVRAM),
	}
	if in.ComposeRaw != "" {
		r.ComposeCommand, r.EnvChecks = resolveCompose(in.ServiceName, in.ComposeRaw, in.ResolvedEnv, in.EnvFileKeys)
	}
	r.MostLikelyCause, r.SuggestedAction = decide(r, in)
	return r
}

func boundLogTail(s string) string {
	if len(s) <= maxLogTailBytes {
		return s
	}
	return s[len(s)-maxLogTailBytes:]
}

// redactSecretsFromLog scrubs literal occurrences of secret-looking env
// values out of a log tail: every value whose var name matches secretVarRe.
// Necessarily incomplete -- it only catches secrets citadel itself resolved
// into this compose invocation, not arbitrary secrets an engine might print
// from elsewhere -- but it closes the concrete gap of a value already
// redacted in the compose-command/env-check sections leaking back out
// through the raw log. Values shorter than 6 chars are skipped: redacting
// them would risk mass false-positive matches against ordinary log text.
//
// Deliberately does NOT also treat Input.EnvFileKeys as secret here, unlike
// resolveCompose's redact/isSecretVar: EnvFileKeys marks EVERY var sourced
// from <name>.env, which includes ordinary config (a model name, a port) as
// well as real secrets. Redacting those out of the log tail would mangle
// ExtractRootError's read of the SAME text (e.g. a root error that legitimately
// mentions the model name) -- an honesty cost the EnvChecks/ComposeCommand
// sections don't pay, because there a value is displayed as a value, not
// embedded in prose that gets pattern-matched afterward.
func redactSecretsFromLog(logTail string, env map[string]string) string {
	if logTail == "" {
		return logTail
	}
	for name, val := range env {
		if len(val) < 6 || !secretVarRe.MatchString(name) {
			continue
		}
		logTail = strings.ReplaceAll(logTail, val, "***REDACTED***")
	}
	return logTail
}

// pythonErrLine matches the final line of a Python traceback / structured
// error message: "SomeError: message" or "some.module.SomeException:
// message". Deliberately broad (any identifier ending in
// Error/Exception/Warning followed by ": ") rather than an engine-specific
// parser -- a traceback always ends with the actual error, so scanning from
// the bottom for this shape finds it directly without reconstructing the
// whole stack.
var pythonErrLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*(?:Error|Exception|Warning)\s*:\s*.+$`)

// ExtractRootError finds the single most salient error line in a container
// log tail. It scans from the bottom for a line matching pythonErrLine, and
// falls back to the last non-empty line when nothing matches -- an honest
// "last thing printed before it died" signal rather than a guess. Returns ""
// for an empty log.
func ExtractRootError(logTail string) string {
	lines := strings.Split(logTail, "\n")
	var lastNonEmpty string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if lastNonEmpty == "" {
			lastNonEmpty = truncate(line, 500)
		}
		if pythonErrLine.MatchString(line) {
			return truncate(line, 500)
		}
	}
	return lastNonEmpty
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// hintPattern is a known-failure-mode regex over the raw log with a
// human-readable hint. Deliberately heuristic and engine-agnostic -- these
// are NOT a vLLM-internals parser, just patterns worth surfacing so an
// operator/agent doesn't have to know them by heart.
type hintPattern struct {
	re   *regexp.Regexp
	hint string
}

var hintPatterns = []hintPattern{
	{regexp.MustCompile(`(?i)trust_remote_code`), "model requires --trust-remote-code (or trust_remote_code=true) to load custom code"},
	{regexp.MustCompile(`(?i)\bnot supported\b`), "engine reported an unsupported model/architecture/operation -- check the model is compatible with this engine"},
	{regexp.MustCompile(`(?i)out of memory|CUDA out of memory|\bOOM\b`), "looks like a GPU out-of-memory error -- see the VRAM fit check"},
	{regexp.MustCompile(`(?i)no such file or directory.*\.gguf|does not appear to have a file named|model.*not found`), "a model file/weights could not be found -- check the model path/cache mount"},
	{regexp.MustCompile(`(?i)permission denied`), "a permission error -- check volume/file ownership"},
	{regexp.MustCompile(`(?i)address already in use|port is already allocated`), "the host port is already in use by another process/container"},
}

// DetectHints scans a log tail for known failure-mode patterns, returning a
// hint for each distinct pattern that matched (in fixed priority order).
func DetectHints(logTail string) []string {
	var hints []string
	for _, hp := range hintPatterns {
		if hp.re.MatchString(logTail) {
			hints = append(hints, hp.hint)
		}
	}
	return hints
}

// varToken matches a docker-compose-style ${VAR}, ${VAR:-default}, or
// ${VAR:?message} interpolation token. Deliberately covers only the
// colon-qualified forms actually used by citadel's embedded compose files
// (services/compose/*.yml) -- this is a heuristic extractor, not a full
// compose variable-interpolation implementation.
var varToken = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([-?])([^}]*))?\}`)

// secretVarRe matches env var names that look like they carry a secret, so
// their resolved value is redacted rather than echoed into a report that
// might be pasted into a chat/ticket. This is a NAME-pattern heuristic and
// deliberately not the only redaction path -- see isSecretVar.
var secretVarRe = regexp.MustCompile(`(?i)(token|secret|key|password|passwd|credential|auth)`)

// isSecretVar decides whether name's value should be redacted: either its
// name looks secret-shaped (secretVarRe), or it is explicitly flagged via
// fileKeys. fileKeys exists because name-pattern matching alone is not a safe
// default for values sourced from the install-time <name>.env sibling
// (Input.EnvFileKeys) -- that whole file is documented as secret-bearing
// (catalog installs write API keys there), with no naming convention citadel
// controls, so every key from it is treated as secret regardless of name.
func isSecretVar(name string, fileKeys map[string]bool) bool {
	return secretVarRe.MatchString(name) || fileKeys[name]
}

func redact(name, value string, fileKeys map[string]bool) string {
	if value != "" && isSecretVar(name, fileKeys) {
		return "***REDACTED***"
	}
	return value
}

// composeDoc is the minimal shape resolveCompose needs out of a compose
// file's YAML.
type composeDoc struct {
	Services map[string]struct {
		Command any `yaml:"command"`
	} `yaml:"services"`
}

// resolveCompose extracts the effective (env-interpolated) command for
// serviceName and a verdict for every ${VAR...} token referenced anywhere in
// the raw compose file (not just the command -- a guarded host port usually
// lives in `ports:`). fileKeys marks values sourced from the install-time env
// file for unconditional redaction (see isSecretVar).
func resolveCompose(serviceName, raw string, env map[string]string, fileKeys map[string]bool) (string, []EnvCheck) {
	checks := checkEnvVars(raw, env, fileKeys)
	cmd := extractCommand(serviceName, raw)
	resolved := interpolate(cmd, env, fileKeys)
	return resolved, checks
}

// extractCommand pulls services.<serviceName>.command out of raw compose
// YAML, normalizing both the block-scalar string form and the exec-array
// form into a single space-joined string. Returns "" if the file can't be
// parsed or declares no command for this service (e.g. it uses the image's
// default entrypoint).
func extractCommand(serviceName, raw string) string {
	var doc composeDoc
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	svc, ok := doc.Services[serviceName]
	if !ok {
		return ""
	}
	switch v := svc.Command.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// checkEnvVars finds every distinct ${VAR...} token in raw and resolves it
// against env, mirroring (a subset of) docker compose's own substitution
// rules: ${VAR:-default} falls back to default when VAR is unset OR empty;
// ${VAR:?msg} is required (VAR must be set AND non-empty); a bare ${VAR} is
// optional with no default.
func checkEnvVars(raw string, env map[string]string, fileKeys map[string]bool) []EnvCheck {
	matches := varToken.FindAllStringSubmatch(raw, -1)
	seen := map[string]bool{}
	var out []EnvCheck
	for _, m := range matches {
		name, op, rest := m[1], m[2], m[3]
		if seen[name] {
			continue
		}
		seen[name] = true

		val, present := env[name]
		ec := EnvCheck{Var: name, Required: op == "?"}
		if op == "-" {
			ec.Default = rest
		}

		switch {
		case present && val != "":
			ec.Value = redact(name, val, fileKeys)
			ec.Verdict = EnvOK
		case ec.Required:
			ec.Verdict = EnvMissing
			if present {
				ec.Verdict = EnvEmpty
			}
		case ec.Default != "":
			ec.Value = redact(name, ec.Default, fileKeys)
			ec.Verdict = EnvOK
		case present:
			ec.Verdict = EnvEmpty
		default:
			ec.Verdict = EnvMissing
		}
		out = append(out, ec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Var < out[j].Var })
	return out
}

// interpolate substitutes every ${VAR...} token in s with its resolved value
// from env, following the same rules as checkEnvVars. An unresolved required
// or bare token is left as a visible placeholder rather than silently
// disappearing, so a broken command is obviously broken when printed.
func interpolate(s string, env map[string]string, fileKeys map[string]bool) string {
	return varToken.ReplaceAllStringFunc(s, func(tok string) string {
		m := varToken.FindStringSubmatch(tok)
		name, op, rest := m[1], m[2], m[3]
		if val, present := env[name]; present && val != "" {
			return redact(name, val, fileKeys)
		}
		if op == "-" {
			return rest
		}
		if op == "?" {
			return "<MISSING:" + name + ">"
		}
		return "<UNSET:" + name + ">"
	})
}

// CheckVRAMFit compares a declared VRAM need against free VRAM. Unknown
// (neither a fit nor a failure) when either side is unavailable -- an absent
// signal must never be read as "fits" or "insufficient".
func CheckVRAMFit(freeMB int, haveFree bool, needMB int, haveNeed bool) VRAMFit {
	v := VRAMFit{FreeMB: freeMB, NeedMB: needMB, HaveFree: haveFree, HaveNeed: haveNeed}
	switch {
	case !haveFree || !haveNeed:
		v.Verdict = VRAMUnknown
	case freeMB >= needMB:
		v.Verdict = VRAMFits
	default:
		v.Verdict = VRAMInsufficient
	}
	return v
}

// decide picks the single most-likely cause + suggested action from
// everything gathered, in a fixed priority order: a hard resource/config
// blocker (VRAM, required env) outranks a log-derived guess, which outranks
// "no container at all", which outranks a generic fallback.
func decide(r Report, in Input) (cause, action string) {
	if r.VRAM.Verdict == VRAMInsufficient {
		return fmt.Sprintf("insufficient free VRAM: %dMB free vs %dMB needed", r.VRAM.FreeMB, r.VRAM.NeedMB),
			"free VRAM by stopping another service ('citadel services' shows usage/footprint) or reduce this service's footprint, then retry"
	}

	for _, ec := range r.EnvChecks {
		if ec.Verdict == EnvMissing || ec.Verdict == EnvEmpty {
			return fmt.Sprintf("required compose variable %s is %s", ec.Var, ec.Verdict),
				fmt.Sprintf("set %s before starting %s (check citadel.yaml / node config), then retry", ec.Var, r.ServiceName)
		}
	}

	if !r.Container.Exists {
		if in.Managed {
			return "service has no container -- it has never been started, or its container was removed",
				fmt.Sprintf("run 'citadel run --service %s' to start it, or 'citadel logs %s' to see a prior start attempt", r.ServiceName, r.ServiceName)
		}
		return fmt.Sprintf("%q is not declared in citadel.yaml's services list and no matching container was found", r.ServiceName),
			"check the service name against 'citadel services' or citadel.yaml"
	}

	// A running container is presumed healthy unless a hard blocker above
	// already fired. This MUST precede the hint scan below: several
	// known-pattern regexes (e.g. `\bnot supported\b`) also match ordinary
	// healthy startup chatter (vLLM logs capability notices like "flash
	// attention 2 not supported for this GPU, falling back to..."), so
	// checking hints first would fabricate a cause for a container that is
	// actually fine.
	if strings.EqualFold(r.Container.Status, "running") {
		return "container is running; no obvious problem detected by this check",
			fmt.Sprintf("if it's still misbehaving, check 'citadel logs %s' or engine-specific health output", r.ServiceName)
	}

	if len(r.Hints) > 0 {
		cause := r.Hints[0]
		if r.RootError != "" {
			cause = fmt.Sprintf("%s (%s)", cause, r.RootError)
		}
		return cause, fmt.Sprintf("see 'citadel logs %s' and the hint(s) above for the full context", r.ServiceName)
	}

	if r.Container.ExitCode != 0 && r.RootError != "" {
		return fmt.Sprintf("container exited with code %d: %s", r.Container.ExitCode, r.RootError),
			fmt.Sprintf("see 'citadel logs %s' for the full traceback", r.ServiceName)
	}
	if r.Container.ExitCode != 0 {
		return fmt.Sprintf("container exited with code %d (no clear error line found in the log tail)", r.Container.ExitCode),
			fmt.Sprintf("see 'citadel logs %s' for more context", r.ServiceName)
	}

	return fmt.Sprintf("container state is %q with no clear root cause found in the log tail", r.Container.Status),
		fmt.Sprintf("see 'citadel logs %s' for details", r.ServiceName)
}
