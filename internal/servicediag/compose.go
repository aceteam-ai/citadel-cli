// internal/servicediag/compose.go
//
// Best-effort, read-only reasoning about a service's compose definition: the
// effective (${VAR}-substituted) command + environment, and which
// ${VAR:?required} host-port-style guards (see services/ports.go) are unmet
// in the resolved env. This never shells out to `docker compose config` --
// diagnose must never write a file (a not-yet-materialized embedded service
// has no on-disk compose to hand docker), so substitution is done directly
// against the raw compose text/YAML.
package servicediag

import (
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeDoc / composeService are a minimal, permissive decode of a citadel
// compose file -- only the fields diagnose reasons about.
type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Command     interface{} `yaml:"command"`
	Environment interface{} `yaml:"environment"`
}

// requiredVarRe matches compose's ${VAR:?message} required-variable guard
// (the pattern services/ports.go's host-port vars use).
var requiredVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):\?([^}]*)\}`)

// varRefRe matches any ${...} interpolation reference, used for best-effort
// substitution when rendering the effective command/environment.
var varRefRe = regexp.MustCompile(`\$\{([^}]*)\}`)

// RequiredVarCheck is one ${VAR:?message} guard found in a compose file and
// its resolution against the resolved env.
type RequiredVarCheck struct {
	Var     string
	Verdict string // VerdictOK | VerdictFail
	Message string // the guard's own :?message text
	Reason  string // "missing" | "empty" | "" (set)
}

// MissingRequiredVars scans composeContent for every ${VAR:?message} guard
// and reports whether VAR is set (and non-empty) in env. Compose's own :?
// guard fails on both "unset" and "empty" (bash semantics), so both count as
// VerdictFail here. Each distinct VAR is reported once, sorted for stable
// output.
func MissingRequiredVars(composeContent string, env map[string]string) []RequiredVarCheck {
	if composeContent == "" {
		return nil
	}
	matches := requiredVarRe.FindAllStringSubmatch(composeContent, -1)
	seen := map[string]bool{}
	var out []RequiredVarCheck
	for _, m := range matches {
		varName, msg := m[1], strings.TrimSpace(m[2])
		if seen[varName] {
			continue
		}
		seen[varName] = true
		val, present := env[varName]
		check := RequiredVarCheck{Var: varName, Message: msg, Verdict: VerdictOK}
		switch {
		case !present:
			check.Verdict, check.Reason = VerdictFail, "missing"
		case strings.TrimSpace(val) == "":
			check.Verdict, check.Reason = VerdictFail, "empty"
		}
		out = append(out, check)
	}
	sortRequiredVarChecks(out)
	return out
}

func sortRequiredVarChecks(checks []RequiredVarCheck) {
	for i := 1; i < len(checks); i++ {
		for j := i; j > 0 && checks[j].Var < checks[j-1].Var; j-- {
			checks[j], checks[j-1] = checks[j-1], checks[j]
		}
	}
}

// substituteVars performs best-effort ${VAR}, ${VAR:-default}, and
// ${VAR:?message} substitution against env. This is NOT a full docker
// compose interpolation engine (no ${VAR-default}/${VAR?message} no-colon
// forms, no nested/escaped `}`), just enough to render a readable "effective
// command" for a human. An unresolved required var renders as
// "<unset:VAR>" so it is obvious in the output rather than silently blank.
func substituteVars(s string, env map[string]string) string {
	if s == "" {
		return s
	}
	return varRefRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-1]
		name := inner
		def := ""
		hasDefault := false
		required := false
		if idx := strings.Index(inner, ":-"); idx >= 0 {
			name, def, hasDefault = inner[:idx], inner[idx+2:], true
		} else if idx := strings.Index(inner, ":?"); idx >= 0 {
			name, required = inner[:idx], true
		}
		if val, ok := env[name]; ok && val != "" {
			return val
		}
		if hasDefault {
			return def
		}
		if required {
			return "<unset:" + name + ">"
		}
		return ""
	})
}

// commandString normalizes a compose `command:` value (a plain/folded string
// or a list form) into a single display string.
func commandString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, p := range t {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// environmentMap normalizes a compose `environment:` value (a list of
// "KEY=VALUE" strings, or a KEY: value mapping) into a map.
func environmentMap(v interface{}) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case []interface{}:
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				continue
			}
			if k, val, found := strings.Cut(s, "="); found {
				out[strings.TrimSpace(k)] = val
			}
		}
	case map[string]interface{}:
		for k, val := range t {
			if s, ok := val.(string); ok {
				out[k] = s
			} else {
				out[k] = toDisplayString(val)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toDisplayString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return yamlScalar(t)
	}
}

// yamlScalar renders a non-string scalar (bool/int/float) the same way YAML
// would print it, without pulling in fmt.Sprintf("%v") edge cases for every
// call site.
func yamlScalar(v interface{}) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// buildComposeInfo parses in.ComposeContent (if any) and renders the
// effective command + environment for in.ServiceName, substituted against
// in.ResolvedEnv and redacted. Degrades to an empty-but-valid ComposeInfo
// when there is no content, the service key isn't found, or parsing fails --
// never panics.
func buildComposeInfo(in Input) ComposeInfo {
	ci := ComposeInfo{ComposeFilePath: in.ComposeFilePath, Source: in.ComposeSource}
	if len(in.ComposeContent) == 0 {
		return ci
	}
	var doc composeDoc
	if err := yaml.Unmarshal(in.ComposeContent, &doc); err != nil {
		ci.ParseError = err.Error()
		return ci
	}
	svc, ok := doc.Services[in.ServiceName]
	if !ok {
		// A compose file with exactly one service is unambiguous even when
		// its key doesn't literally match the manifest/catalog service name
		// (defensive -- every embedded/known compose today does match).
		if len(doc.Services) == 1 {
			for _, v := range doc.Services {
				svc, ok = v, true
			}
		}
	}
	if !ok {
		return ci
	}
	// Command is a single interpolated string (not a key/value map), so it
	// can't be redacted by RedactEnv's key-name pass -- a compose command:
	// like "--hf-token ${HF_TOKEN}" renders the literal secret into the
	// string with no variable name left in it. RedactText scrubs by VALUE
	// instead, using the same secret-shaped-key heuristic to decide which
	// resolved env values count as secrets.
	ci.Command = RedactText(substituteVars(commandString(svc.Command), in.ResolvedEnv), in.ResolvedEnv)
	rawEnv := environmentMap(svc.Environment)
	if rawEnv != nil {
		resolved := make(map[string]string, len(rawEnv))
		for k, v := range rawEnv {
			// RedactEnv below only catches a secret by its OWN key name. A
			// non-secret-shaped key can still interpolate another var's
			// secret value into itself (e.g.
			// MODEL_URL=https://user:${HF_TOKEN}@host/model) -- RedactText
			// closes that gap by value, same as Command above, before the
			// key-name pass runs.
			resolved[k] = RedactText(substituteVars(v, in.ResolvedEnv), in.ResolvedEnv)
		}
		ci.Env = RedactEnv(resolved)
	}
	return ci
}

// ParseDotEnv parses simple KEY=VALUE lines (docker compose --env-file
// format: blank lines and #-prefixed comments ignored, no export/quoting
// support needed for citadel's own generated env files). Used to fold a
// service's sibling <name>.env into the resolved env before process-env
// overrides are applied (process env wins, mirroring docker compose's own
// precedence).
func ParseDotEnv(content []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if k, v, found := strings.Cut(trimmed, "="); found {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
