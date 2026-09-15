// services/bind.go
//
// Host-bind resolution for the embedded engine compose files
// (aceteam-ai/citadel-cli#1023). aceteam-ai/aceteam#9523 (PR #1025) defaulted
// vllm/llamacpp/bonsai/unlimited-ocr/sglang to a loopback-only host publish
// (127.0.0.1) because they are OpenAI-compatible servers with no auth of their
// own. This file adds the per-service escape hatch that issue's acceptance list
// asked for: an operator can opt an engine back into an all-interfaces (0.0.0.0)
// publish via a `bind:` field on the service in citadel.yaml.
//
// Mechanism (the issue's "two-substitution compose form"): each hatch engine's
// compose port publish is
//
//	${CITADEL_<SVC>_BIND:-<default>}:${CITADEL_<SVC>_HOST_PORT}:<cport>
//
// (or a literal host port for sglang/ollama, which have no ${...HOST_PORT} var).
// The embedded template stays the single source of truth; citadel injects
// CITADEL_<SVC>_BIND ONLY when the manifest `bind:` field is explicitly set, so
// an unset field falls through to the compose `:-` default:
//
//   - the 5 no-auth engines default to 127.0.0.1 (loopback),
//   - ollama defaults to 0.0.0.0 because at least one catalog app reaches it via
//     host.docker.internal, which lands on the docker0 bridge gateway a loopback
//     publish would refuse (see services/compose/ollama.yml).
//
// This file is the single authority both the loopback host-port test parsers
// (services/embed_test.go, internal/apps/hostport_collision_test.go), the #1030
// bind-drift loop-guard (cmd/compose_refresh.go), and the doctor/status bind
// warning resolve through, so none of them can silently disagree about a
// template's effective bind.
package services

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Host bind addresses the manifest `bind:` field resolves to.
const (
	// LoopbackBindAddr is the default host bind for the no-auth inference
	// engines (aceteam-ai/aceteam#9523): reachable only from the node itself.
	LoopbackBindAddr = "127.0.0.1"
	// AllInterfacesBindAddr is the escape-hatch host bind (`bind: all`):
	// reachable from any interface, including the LAN.
	AllInterfacesBindAddr = "0.0.0.0"
)

// Bind env-var names referenced by the two-substitution compose form in
// services/compose/*.yml. Kept as exported constants so the compose templates
// and the Go code that injects them share one spelling.
const (
	EnvVLLMBind         = "CITADEL_VLLM_BIND"
	EnvLlamacppBind     = "CITADEL_LLAMACPP_BIND"
	EnvBonsaiBind       = "CITADEL_BONSAI_BIND"
	EnvUnlimitedOCRBind = "CITADEL_UNLIMITED_OCR_BIND"
	EnvSGLangBind       = "CITADEL_SGLANG_BIND"
	EnvOllamaBind       = "CITADEL_OLLAMA_BIND"
)

// serviceBindEnv maps each embedded ServiceMap engine that supports the
// manifest `bind:` escape hatch to its compose bind env var. It is deliberately
// NARROWER than ServiceMap: kokoro/omnivoice are co-located-consumer-only and
// must never be reachable off-host (see their compose comments), and
// extraction/diffusers/transcribe/lmstudio are the deferred loopback sweep
// (still all-interfaces in v1), so none of them carry the hatch.
var serviceBindEnv = map[string]string{
	"vllm":          EnvVLLMBind,
	"llamacpp":      EnvLlamacppBind,
	"bonsai":        EnvBonsaiBind,
	"unlimited-ocr": EnvUnlimitedOCRBind,
	"sglang":        EnvSGLangBind,
	"ollama":        EnvOllamaBind,
}

// BindEnvVarName returns the compose bind env var for a hatch-supporting
// service and whether one is registered.
func BindEnvVarName(service string) (string, bool) {
	v, ok := serviceBindEnv[service]
	return v, ok
}

// ResolveBindAddr maps a manifest `bind:` field value to the host bind address
// the CITADEL_<SVC>_BIND env var carries:
//
//	""                                     -> ("", nil)  // unset: inject nothing,
//	                                                      // the compose :- default wins
//	"all" / "0.0.0.0" / "*"                -> ("0.0.0.0", nil)
//	"loopback" / "localhost" / "127.0.0.1" -> ("127.0.0.1", nil)
//	anything else                          -> ("", error) // typo: refuse loudly
//
// A typo silently resolving to the safe default would be confusing, and
// silently resolving to all-interfaces would be dangerous, so an unrecognized
// value is an error the caller surfaces rather than guessing.
func ResolveBindAddr(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", nil
	case "all", "0.0.0.0", "*", "any", "all-interfaces":
		return AllInterfacesBindAddr, nil
	case "loopback", "localhost", "127.0.0.1", "local":
		return LoopbackBindAddr, nil
	default:
		return "", fmt.Errorf("unrecognized bind value %q (use \"all\" for all interfaces or \"loopback\" for localhost-only)", v)
	}
}

// BindEnv returns the "CITADEL_<SVC>_BIND=<addr>" entry to inject for a service
// given its manifest `bind:` value, and whether to inject it. It returns
// ("", false, nil) when the service is not hatch-capable OR the bind value is
// unset (so the compose `:-` default applies). An unrecognized value is an
// error.
func BindEnv(service, manifestBind string) (string, bool, error) {
	name, ok := BindEnvVarName(service)
	if !ok {
		return "", false, nil
	}
	addr, err := ResolveBindAddr(manifestBind)
	if err != nil {
		return "", false, err
	}
	if addr == "" {
		return "", false, nil
	}
	return name + "=" + addr, true, nil
}

// BindEnvMapForService returns a single-entry env map {VAR: addr} for a
// service's manifest bind value, suitable for ResolvePortSpecBind /
// ComposePublishesLoopbackOnly, or nil when nothing should be injected (not
// hatch-capable, or bind unset). An unrecognized value is an error.
func BindEnvMapForService(service, manifestBind string) (map[string]string, error) {
	name, ok := BindEnvVarName(service)
	if !ok {
		return nil, nil
	}
	addr, err := ResolveBindAddr(manifestBind)
	if err != nil {
		return nil, err
	}
	if addr == "" {
		return nil, nil
	}
	return map[string]string{name: addr}, nil
}

// splitComposePortFields splits a docker-compose short-syntax `ports:` entry
// into its colon-separated fields, treating a ${...} expansion (which may carry
// its own ':' from a :-/:? operator) as ONE atomic field. This is what lets the
// two-substitution form ${BIND:-127.0.0.1}:${HOST}:8000 parse correctly where a
// naive strings.Split on ':' would shatter both expansions.
func splitComposePortFields(spec string) []string {
	var fields []string
	var cur strings.Builder
	for i := 0; i < len(spec); {
		if spec[i] == '$' && i+1 < len(spec) && spec[i+1] == '{' {
			end := strings.IndexByte(spec[i:], '}')
			if end < 0 {
				cur.WriteString(spec[i:])
				break
			}
			cur.WriteString(spec[i : i+end+1])
			i += end + 1
			continue
		}
		if spec[i] == ':' {
			fields = append(fields, cur.String())
			cur.Reset()
			i++
			continue
		}
		cur.WriteByte(spec[i])
		i++
	}
	fields = append(fields, cur.String())
	return fields
}

// ComposePortHostToken returns the host-port field of a compose short-syntax
// port entry (the field immediately before the container port), or "" when the
// entry declares no host publish (a bare container port). It handles every form
// the embedded templates use: HOST:CONTAINER, IP:HOST:CONTAINER,
// ${VAR:?msg}:CONTAINER, 127.0.0.1:${VAR}:CONTAINER, and the #1023
// ${BIND:-127.0.0.1}:${VAR}:CONTAINER two-substitution form.
func ComposePortHostToken(spec string) string {
	fields := splitComposePortFields(spec)
	switch len(fields) {
	case 0, 1:
		return "" // container-port-only, no host publish
	case 2:
		return fields[0] // HOST:CONTAINER
	default:
		// IP:HOST:CONTAINER (the compose files here use an IPv4 / ${...} IP, so
		// the host port is always the second-to-last field).
		return fields[len(fields)-2]
	}
}

// resolveComposeToken resolves a single compose field to its literal value: a
// ${VAR}/${VAR:-default}/${VAR:?msg} expansion is resolved against env (VAR
// present and non-empty -> env[VAR], else the :- default, else ""); a literal is
// returned unchanged.
func resolveComposeToken(tok string, env map[string]string) string {
	if strings.HasPrefix(tok, "${") && strings.HasSuffix(tok, "}") {
		inner := tok[2 : len(tok)-1]
		name := inner
		def := ""
		if i := strings.IndexByte(inner, ':'); i >= 0 {
			name = inner[:i]
			if strings.HasPrefix(inner[i:], ":-") {
				def = inner[i+2:]
			}
			// ":?msg" (required) and ":default" (rare) carry no usable default
			// for a bind field; the templates only ever use :- for the bind var.
		}
		if v, ok := env[name]; ok && v != "" {
			return v
		}
		return def
	}
	return tok
}

// ResolvePortSpecBind returns the effective host bind address of one compose
// port entry, resolving a ${BIND:-default} IP field against env, plus whether
// the entry publishes a host port at all:
//
//   - IP:HOST:CONTAINER -> (resolved IP, true)
//   - HOST:CONTAINER    -> ("", true)   // no explicit IP: docker binds all interfaces
//   - CONTAINER         -> ("", false)  // no host publish
//
// An empty bind address on a published entry therefore means "all interfaces",
// which is exactly what IsLoopbackBindAddr treats as non-loopback.
func ResolvePortSpecBind(spec string, env map[string]string) (bind string, hasPublish bool) {
	fields := splitComposePortFields(spec)
	switch len(fields) {
	case 0, 1:
		return "", false
	case 2:
		return "", true
	default:
		return resolveComposeToken(fields[0], env), true
	}
}

// IsLoopbackBindAddr reports whether a resolved host bind address is a loopback
// address (reachable only from the node itself). An empty address (a
// HOST:CONTAINER publish with no explicit IP) is NOT loopback -- docker binds it
// on all interfaces.
func IsLoopbackBindAddr(addr string) bool {
	switch strings.TrimSpace(addr) {
	case LoopbackBindAddr, "::1":
		return true
	default:
		return false
	}
}

// ComposePublishesLoopbackOnly reports whether, under env, EVERY host publish in
// composeContent binds a loopback address (loopbackOnly), and whether there is
// any host publish at all (hasPublish). It is the single authority the #1023
// bind hatch, the #1030 drift loop-guard, and TestServiceMapBindSweep resolve
// through. loopbackOnly is false when there is no host publish (there is nothing
// to be loopback-only about); callers that need to distinguish "no publish" from
// "published on all interfaces" read hasPublish.
func ComposePublishesLoopbackOnly(composeContent string, env map[string]string) (loopbackOnly, hasPublish bool) {
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeContent), &doc); err != nil {
		return false, false
	}
	allLoopback := true
	for _, svc := range doc.Services {
		for _, spec := range svc.Ports {
			bind, has := ResolvePortSpecBind(spec, env)
			if !has {
				continue
			}
			hasPublish = true
			if !IsLoopbackBindAddr(bind) {
				allLoopback = false
			}
		}
	}
	return hasPublish && allLoopback, hasPublish
}
