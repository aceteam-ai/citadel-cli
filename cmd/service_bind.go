// cmd/service_bind.go
//
// Per-service host-bind escape hatch wiring (aceteam-ai/citadel-cli#1023). The
// pure resolution lives in services/bind.go; this file is the cmd-side glue that
// reads the manifest `bind:` field, turns it into the CITADEL_<SVC>_BIND env the
// compose ${CITADEL_<SVC>_BIND:-...} substitution consumes, and answers "is this
// engine going to be published on all interfaces?" for the start warning and the
// doctor/status warning.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// manifestServiceFor returns the manifest Service entry for serviceName (and
// whether one was found). Best-effort and --node-dir-aware (it reads through
// findAndReadManifest); any manifest read error yields (zero, false) so callers
// fall through to the compose defaults rather than failing. The single manifest
// read both the #1023 bind hatch and the #1209 ollama policy derive from, so a
// start path does not read citadel.yaml twice.
func manifestServiceFor(serviceName string) (Service, bool) {
	manifest, _, err := findAndReadManifest()
	if err != nil || manifest == nil {
		return Service{}, false
	}
	for _, s := range manifest.Services {
		if s.Name == serviceName {
			return s, true
		}
	}
	return Service{}, false
}

// manifestServiceBind returns the manifest `bind:` value for serviceName, or ""
// if unresolvable/unset. Best-effort and --node-dir-aware; any manifest read
// error yields "" so the compose `:-` default applies rather than failing the
// caller.
func manifestServiceBind(serviceName string) string {
	s, ok := manifestServiceFor(serviceName)
	if !ok {
		return ""
	}
	return s.Bind
}

// serviceBindEnvMap resolves the single-entry {CITADEL_<SVC>_BIND: addr} map for
// a service's manifest bind value, or nil when nothing should be injected (not a
// hatch-capable service, or bind unset so the compose default wins). An
// unrecognized bind value is a hard error the caller surfaces (never a silent
// fall-through to the safe default). It wraps services.BindEnvMapForService so
// this is the ONE cmd-side resolution point.
func serviceBindEnvMap(serviceName, manifestBind string) (map[string]string, error) {
	return svcports.BindEnvMapForService(serviceName, manifestBind)
}

// bindEnvEntries flattens a bind env map into "KEY=value" entries for appending
// to a compose invocation's environment. nil/empty map -> nil.
func bindEnvEntries(bindEnv map[string]string) []string {
	if len(bindEnv) == 0 {
		return nil
	}
	entries := make([]string, 0, len(bindEnv))
	for k, v := range bindEnv {
		entries = append(entries, k+"="+v)
	}
	return entries
}

// composeEnvForService returns composeEnv() plus the manifest-derived
// CITADEL_<SVC>_BIND entry (best-effort). Used by the boot-time port-drift
// recreate path (enginePortRecreator), which knows the service name but not its
// resolved bind. An unrecognized bind value is logged and dropped here (the
// compose `:-` default then applies) rather than failing a best-effort recreate;
// startService/serviceStart, the paths where an operator can act on the error,
// refuse loudly instead.
func composeEnvForService(serviceName string) []string {
	svc, _ := manifestServiceFor(serviceName)
	return composeEnvForServiceValues(serviceName, svc)
}

// composeEnvForServiceValues is the hermetic core of composeEnvForService. It
// keeps the port-drift/restart builder testable without resolving the live node
// manifest, while production still supplies the one entry read above.
func composeEnvForServiceValues(serviceName string, svc Service) []string {
	env := composeEnv()
	bindEnv, err := serviceBindEnvMap(serviceName, svc.Bind)
	if err != nil {
		Log("bind: %s: %v; using compose default", serviceName, err)
	} else {
		env = append(env, bindEnvEntries(bindEnv)...)
	}
	// Normalize the #1209 Ollama policy LAST. Even nil/unreadable/invalid
	// best-effort policy becomes an explicit empty entry for ollama so neither an
	// inherited shell value nor Compose .env fallback can become node policy.
	env, policyErr := svcports.OllamaMaxLoadedModelsComposeEnv(env, serviceName, svc.OllamaMaxLoadedModels)
	if policyErr != nil {
		Log("ollama policy: %s: %v; using engine default", serviceName, policyErr)
	}
	// aceteam-ai/citadel-cli#1269: GPU-aware tei image tag / CPU thread opt-in.
	// No-op for every non-tei service.
	env = append(env, teiComposeEnvEntries(serviceName, svc.Threads)...)
	return env
}

// engineExposedOnAllInterfaces reports whether an embedded engine service, as it
// will actually be published given its manifest bind (bindEnv) and its compose
// content, binds a host port on all interfaces (LAN-reachable). It scopes to
// embedded ServiceMap engines: a catalog/third-party module authors its own
// bind and is out of scope for the #1023 exposure warning.
func engineExposedOnAllInterfaces(serviceName, composeContent string, bindEnv map[string]string) bool {
	if !isEmbeddedService(serviceName) {
		return false
	}
	loopbackOnly, hasPublish := svcports.ComposePublishesLoopbackOnly(composeContent, bindEnv)
	return hasPublish && !loopbackOnly
}

// engineBindExposureWarning returns a human-readable warning line if the embedded
// engine will be published on all interfaces, or "" otherwise. Shared by the
// service-start warning and the doctor/status warning so the wording is one
// place.
func engineBindExposureWarning(serviceName, composeContent string, bindEnv map[string]string) string {
	if !engineExposedOnAllInterfaces(serviceName, composeContent, bindEnv) {
		return ""
	}
	return fmt.Sprintf(
		"%s is published on ALL network interfaces (0.0.0.0) with no authentication of its own; "+
			"anyone who can reach this host on its port can use it. Set `bind: loopback` on the service "+
			"in citadel.yaml to restrict it to localhost.",
		serviceName)
}

// composeContentForService returns the compose content that will actually be
// used for a manifest service: the materialized <configDir>/<ComposeFile> when
// present (so a hand-edit is reflected), falling back to the embedded template.
func composeContentForService(s Service, configDir string) string {
	if s.ComposeFile != "" {
		if data, err := os.ReadFile(filepath.Join(configDir, s.ComposeFile)); err == nil {
			return string(data)
		}
	}
	if content, ok := svcports.ServiceMap[s.Name]; ok {
		return content
	}
	return ""
}

// serviceBindExposureWarning returns the #1023 exposure warning for a manifest
// service (empty when it is not an embedded engine, has no host publish, or is
// loopback-bound). Shared by `citadel doctor` and `citadel status`.
func serviceBindExposureWarning(s Service, configDir string) string {
	content := composeContentForService(s, configDir)
	if content == "" {
		return ""
	}
	// An unrecognized bind value is surfaced at start (a start failure), not
	// here; evaluate exposure against the compose default in that case.
	bindEnv, _ := serviceBindEnvMap(s.Name, s.Bind)
	return engineBindExposureWarning(s.Name, content, bindEnv)
}

// engineBindExposureWarnings reads the node manifest (best-effort, --node-dir
// aware) and returns one #1023 exposure warning per embedded engine that will be
// published on all interfaces, in manifest order. Empty when no manifest, or
// nothing exposed.
func engineBindExposureWarnings() []string {
	manifest, configDir, err := findAndReadManifest()
	if err != nil || manifest == nil {
		return nil
	}
	var out []string
	for _, s := range manifest.Services {
		if w := serviceBindExposureWarning(s, configDir); w != "" {
			out = append(out, w)
		}
	}
	return out
}
