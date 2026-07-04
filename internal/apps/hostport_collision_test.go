package apps

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// TestHostPortNoCollisions is the guarantee that "containers can't dictate the
// host port". It builds the union of every host port claimed across:
//
//   - the citadel-owned service registry (services.ServiceHostPorts),
//   - the apps catalog's suggested host ports (builtinCatalog),
//   - every services/compose/*.yml file (parsed from the embedded ServiceMap),
//
// and asserts:
//
//  1. no two of those claim the same host port (pairwise-unique), and
//  2. none of them intersect citadel's own reserved ports
//     (services.ReservedCitadelPorts), which are shared across citadel-internal
//     run contexts (gateway/status on 8080, etc.) and must never be taken by a
//     module or app.
//
// Reserved ports are a set-to-AVOID, not a set-to-dedupe: 8080 is intentionally
// reused by the gateway, the status server, and the whatsapp bridge across
// different contexts, so it is not part of the pairwise-uniqueness check.
func TestHostPortNoCollisions(t *testing.T) {
	// owner records who claimed a given host port, so a collision message is
	// actionable.
	owner := map[int]string{}
	reserved := services.ReservedCitadelPorts

	claim := func(port int, who string) {
		t.Helper()
		if port == 0 {
			return
		}
		if name, taken := reserved[port]; taken {
			t.Errorf("host port %d claimed by %q collides with reserved citadel port %q", port, who, name)
			return
		}
		if prev, exists := owner[port]; exists {
			t.Errorf("host port %d claimed by both %q and %q", port, prev, who)
			return
		}
		owner[port] = who
	}

	// 1. Apps catalog suggested host ports (the map VALUE is the suggested host
	// port; the KEY is the container port).
	for name, manifest := range builtinCatalog {
		for _, hostPort := range manifest.Ports {
			claim(hostPort, "apps:"+name)
		}
	}

	// 2. Published host ports parsed out of every embedded compose file. The
	// compose files are the authoritative published set; for the citadel-owned
	// services their host port is resolved from the registry during parsing
	// (see publishedHostPorts), so we do not claim the registry separately here
	// (that would be a self-collision). Instead we assert below that each
	// registry entry matches what its compose file publishes.
	composePorts := map[string]int{}
	for name, composeYAML := range services.ServiceMap {
		ports, err := publishedHostPorts(composeYAML)
		if err != nil {
			t.Fatalf("parse compose %q: %v", name, err)
		}
		for _, port := range ports {
			claim(port, "compose:"+name)
		}
		if len(ports) == 1 {
			composePorts[name] = ports[0]
		}
	}

	// 3. The registry must agree with what each managed service's compose file
	// actually publishes — otherwise the env-var substitution and the Go
	// consumers would target different ports.
	for svc, port := range services.ServiceHostPorts {
		if cp, ok := composePorts[svc]; ok && cp != port {
			t.Errorf("registry host port for %q is %d but its compose publishes %d", svc, port, cp)
		}
	}
}

// composeFile is the minimal shape needed to read published ports from a
// docker-compose YAML.
type composeFile struct {
	Services map[string]struct {
		Ports []string `yaml:"ports"`
	} `yaml:"services"`
}

// publishedHostPorts extracts the HOST side of every `ports:` publish in a
// compose file. Entries whose host port is supplied by a ${CITADEL_*_HOST_PORT}
// env var are resolved against the registry so the test validates the value
// citadel actually injects (this is what proves the deferral is wired to a real
// port). Entries with a literal host port are returned as-is.
func publishedHostPorts(composeYAML string) ([]int, error) {
	var cf composeFile
	if err := yaml.Unmarshal([]byte(composeYAML), &cf); err != nil {
		return nil, err
	}

	// Map each ${VAR} spelling to its registry value.
	envValue := map[string]int{
		services.EnvLlamacppHostPort:   services.LlamacppHostPort,
		services.EnvVLLMHostPort:       services.VLLMHostPort,
		services.EnvExtractionHostPort: services.ExtractionHostPort,
		services.EnvDiffusersHostPort:  services.DiffusersHostPort,
		services.EnvClaudecodeHostPort: services.ClaudecodeHostPort,
	}

	var out []int
	for _, svc := range cf.Services {
		for _, spec := range svc.Ports {
			hostPart := hostPortField(spec)
			if hostPart == "" {
				continue
			}
			if strings.HasPrefix(hostPart, "${") && strings.HasSuffix(hostPart, "}") {
				inner := strings.TrimSuffix(strings.TrimPrefix(hostPart, "${"), "}")
				// Strip any compose default/required suffix, e.g. VAR:?msg or
				// VAR:-default, leaving the bare variable name.
				varName := inner
				if idx := strings.Index(inner, ":"); idx >= 0 {
					varName = inner[:idx]
				}
				port, ok := envValue[varName]
				if !ok {
					return nil, fmt.Errorf("compose publishes unknown host-port var %q; add it to the registry", hostPart)
				}
				out = append(out, port)
				continue
			}
			port, err := strconv.Atoi(hostPart)
			if err != nil {
				return nil, fmt.Errorf("unparseable host port %q in spec %q", hostPart, spec)
			}
			out = append(out, port)
		}
	}
	return out, nil
}

// hostPortField extracts the host-port token from a compose short-syntax ports
// entry. Handles the forms:
//
//	"HOST:CONTAINER"
//	"IP:HOST:CONTAINER"
//	"CONTAINER"                        (no host publish -> "")
//	"${VAR:?msg}:CONTAINER"            (env var host port whose default/required
//	                                    suffix itself contains colons)
//
// The host field may be a `${...}` expansion that contains its own colons
// (compose's `:?`/`:-` operators), so we peel off a leading `${...}` group
// intact before splitting the remainder on ":".
func hostPortField(spec string) string {
	if strings.HasPrefix(spec, "${") {
		if end := strings.Index(spec, "}"); end >= 0 {
			// The `${...}` group is the host field. Anything after it is
			// ":CONTAINER" (and possibly a bind-mode suffix we ignore).
			return spec[:end+1]
		}
		return spec
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		// Only a container port; no host publish.
		return ""
	case 2:
		// HOST:CONTAINER
		return parts[0]
	default:
		// IP:HOST:CONTAINER (IP may itself contain colons for IPv6, but the
		// compose files here use IPv4 / no IP, so the host port is the
		// second-to-last field).
		return parts[len(parts)-2]
	}
}
