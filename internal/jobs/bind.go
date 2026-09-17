// internal/jobs/bind.go
//
// Shared helper for the two secondary embedded-engine start paths that do NOT
// go through ServiceHandler.serviceStart (the aceteam-ai/citadel-cli#1023 bind
// hatch's primary injection site): the legacy llama.cpp model-swap restart
// (llamacpp_inference.go) and the APPLY_DEVICE_CONFIG compose-up
// (config_handler.go). Both fail SAFE without this (the compose ${...:-127.0.0.1}
// default keeps them loopback-only), but an operator's `bind: all` opt-in would
// be silently dropped there -- aceteam-ai/citadel-cli#1060 part 2. This threads
// the manifest bind value into the CITADEL_<SVC>_BIND env so those paths honor
// it too.
package jobs

import (
	"os"

	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// manifestServiceBindFromFile reads the manifest at manifestPath (best-effort)
// and returns the aceteam-ai/citadel-cli#1023 `bind:` value for serviceName, or
// "" when the file is unreadable/unparseable, the service is absent, or its bind
// is unset. A "" result lets the caller fall through to the compose loopback
// default rather than failing a start.
func manifestServiceBindFromFile(manifestPath, serviceName string) string {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ""
	}
	var doc struct {
		Services []struct {
			Name string `yaml:"name"`
			Bind string `yaml:"bind"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ""
	}
	for _, s := range doc.Services {
		if s.Name == serviceName {
			return s.Bind
		}
	}
	return ""
}

// bindEnvForService returns the "CITADEL_<SVC>_BIND=<addr>" entries to append to
// a `docker compose up` invocation's env for serviceName given its manifest
// bind value, or nil to inject nothing (not hatch-capable, bind unset, or an
// unrecognized bind value). Unlike ServiceHandler.serviceStart -- an
// operator-facing path where an unrecognized value is surfaced as a start error
// -- these two secondary paths are best-effort: a typo degrades to the compose
// loopback default rather than failing the start, matching cmd's
// composeEnvForService posture.
func bindEnvForService(serviceName, manifestBind string) []string {
	entry, inject, err := services.BindEnv(serviceName, manifestBind)
	if err != nil || !inject {
		return nil
	}
	return []string{entry}
}
