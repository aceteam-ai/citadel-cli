// internal/servicediag/rootcause.go
//
// Heuristic extraction of the salient error line from a container's log tail,
// plus cheap, honest error-pattern hints. Deliberately shallow: this package
// does not parse engine internals (vLLM/llama.cpp/etc stack traces), it only
// looks for the shape a root-cause line usually takes. "See the log tail
// above" is an acceptable, expected fallback answer.
package servicediag

import (
	"regexp"
	"strings"
)

// rootErrorPatterns are tried, in order, against each line scanning
// BACKWARDS from the end of the tail (the most recent lines are the most
// likely to be the actual failure, not an earlier retry/warning). The first
// match wins.
var rootErrorPatterns = []*regexp.Regexp{
	// A Python traceback's final line: "SomeError: message" or
	// "pkg.module.SomeException: message", starting at column 0 (no leading
	// whitespace -- that's what distinguishes the actual raised exception
	// from an indented traceback frame).
	regexp.MustCompile(`^[A-Za-z_][\w.]*(Error|Exception)\b.*$`),
	// A generic "Error:"/"error:" prefixed message from any engine/tool.
	regexp.MustCompile(`(?i)\berror\s*[:\]]`),
	regexp.MustCompile(`(?i)\bvalidationerror\b`),
	regexp.MustCompile(`(?i)\bfatal\b`),
	regexp.MustCompile(`(?i)\btraceback\b`),
}

// ExtractRootError scans a log tail for the single most salient error line:
// a Python traceback's final Error:/Exception line, or another line matching
// a common error shape, preferring the LAST such match (closest to the
// container's actual exit). Falls back to the last non-empty line when
// nothing matches a known error shape. Returns "" for an empty tail.
func ExtractRootError(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(stripANSI(lines[i]))
		if line == "" {
			continue
		}
		for _, re := range rootErrorPatterns {
			if re.MatchString(line) {
				return line
			}
		}
	}
	// Fallback: the last non-empty line, honestly labeled by the caller as a
	// heuristic guess, not a confirmed root cause.
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(stripANSI(lines[i]))
		if line != "" {
			return line
		}
	}
	return ""
}

// ansiRe strips ANSI color/cursor escape sequences some engines emit, so they
// don't interfere with pattern matching or make the printed root error noisy.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// hintRule is one cheap, best-effort error-pattern hint.
type hintRule struct {
	name string
	re   *regexp.Regexp
	hint string
}

// hintRules are intentionally shallow string-pattern checks, NOT an attempt
// to parse vLLM/llama.cpp/etc internals -- per citadel #852's explicit
// guardrail. Each hint is phrased as a possibility ("may need"/"looks like"),
// never a certainty.
var hintRules = []hintRule{
	{
		name: "oom",
		re:   regexp.MustCompile(`(?i)(out of memory|cuda out of memory|outofmemoryerror|oom[- ]?killed|killed process)`),
		hint: "log mentions an out-of-memory condition -- see the VRAM fit check above and consider a smaller model/context, or freeing VRAM from another service",
	},
	{
		name: "trust_remote_code",
		re:   regexp.MustCompile(`(?i)trust_remote_code`),
		hint: "log mentions trust_remote_code -- this model may need --trust-remote-code (or the engine's equivalent flag) to load custom model code",
	},
	{
		name: "model_not_supported",
		re:   regexp.MustCompile(`(?i)(newmodel|architecture .* not supported|unsupported model|model.*not supported)`),
		hint: "log suggests the model architecture may not be supported by this command -- double-check the model type matches the service (e.g. an embedding model served with a chat-completions command)",
	},
	{
		name: "port_in_use",
		re:   regexp.MustCompile(`(?i)(address already in use|bind:.*already in use)`),
		hint: "log mentions a port already in use -- another process/container may already be bound to the host port",
	},
	{
		name: "permission_denied",
		re:   regexp.MustCompile(`(?i)permission denied`),
		hint: "log mentions a permission error -- check volume mounts and (for GPU services) device permissions",
	},
}

// ErrorHints returns cheap, best-effort hints for known error patterns found
// anywhere in the log tail. Order matches hintRules (stable). Never claims
// certainty -- these are surfaced as hints, not verdicts. Returns nil when
// nothing matched, which is an expected, honest outcome.
func ErrorHints(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	text := strings.Join(lines, "\n")
	var hints []string
	for _, r := range hintRules {
		if r.re.MatchString(text) {
			hints = append(hints, r.hint)
		}
	}
	return hints
}
