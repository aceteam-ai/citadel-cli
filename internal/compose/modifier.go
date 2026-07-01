// Package compose provides utilities for modifying Docker Compose files.
package compose

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostPorts extracts the published HOST ports declared in a Docker Compose file's
// services. It parses the short-form `ports` entries ("HOST:CONTAINER",
// "HOST:CONTAINER/proto", "IP:HOST:CONTAINER", or a bare "CONTAINER" which
// publishes an ephemeral host port and is skipped) as well as the long-form
// mapping ({published: HOST, target: CONTAINER}).
//
// It underpins the citadel-cli#415 assertion that a SERVICE_START container comes
// up with its compose-declared host port actually published: given the embedded
// diffusers compose (`ports: ["8102:7860"]`) it returns [8102]. It is pure and
// daemon-free, so callers can verify the intended publish without a live docker.
func HostPorts(content []byte) []int {
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil
	}

	servicesRaw, ok := doc["services"].(map[string]any)
	if !ok {
		return nil
	}

	var ports []int
	seen := make(map[int]bool)
	add := func(p int) {
		if p > 0 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}

	for _, svcRaw := range servicesRaw {
		svc, ok := svcRaw.(map[string]any)
		if !ok {
			continue
		}
		portsRaw, ok := svc["ports"].([]any)
		if !ok {
			continue
		}
		for _, entry := range portsRaw {
			switch e := entry.(type) {
			case string:
				if p, ok := hostPortFromShortForm(e); ok {
					add(p)
				}
			case int:
				// Bare container port (e.g. `- 8080`) -> ephemeral host port; skip.
			case map[string]any:
				if pub, ok := e["published"]; ok {
					if p, ok := toInt(pub); ok {
						add(p)
					}
				}
			}
		}
	}

	return ports
}

// hostPortFromShortForm parses a short-form compose port mapping and returns the
// host port. Forms: "HOST:CONTAINER", "HOST:CONTAINER/proto", "IP:HOST:CONTAINER".
// A bare "CONTAINER" (no colon) publishes an ephemeral host port and yields ok=false.
func hostPortFromShortForm(spec string) (int, bool) {
	spec = strings.TrimSpace(spec)
	// Drop any /protocol suffix (e.g. "8102:7860/tcp").
	if i := strings.Index(spec, "/"); i >= 0 {
		spec = spec[:i]
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		// Bare container port: ephemeral host publish, not a fixed host port.
		return 0, false
	case 2:
		// HOST:CONTAINER
		return toInt(parts[0])
	case 3:
		// IP:HOST:CONTAINER
		return toInt(parts[1])
	default:
		return 0, false
	}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// StripGPUDevices removes GPU device reservations from a Docker Compose file.
// This is used on non-Linux platforms where NVIDIA Container Toolkit is not available.
// The function removes the deploy.resources.reservations.devices section from each service.
func StripGPUDevices(content []byte) ([]byte, error) {
	// Parse the compose file into a generic map
	var compose map[string]any
	if err := yaml.Unmarshal(content, &compose); err != nil {
		return nil, err
	}

	// Get the services section
	servicesRaw, ok := compose["services"]
	if !ok {
		// No services section, return as-is
		return content, nil
	}

	services, ok := servicesRaw.(map[string]any)
	if !ok {
		return content, nil
	}

	// Iterate through each service and remove GPU device reservations
	for _, serviceRaw := range services {
		service, ok := serviceRaw.(map[string]any)
		if !ok {
			continue
		}

		// Navigate to deploy.resources.reservations.devices and remove it
		deployRaw, ok := service["deploy"]
		if !ok {
			continue
		}

		deploy, ok := deployRaw.(map[string]any)
		if !ok {
			continue
		}

		resourcesRaw, ok := deploy["resources"]
		if !ok {
			continue
		}

		resources, ok := resourcesRaw.(map[string]any)
		if !ok {
			continue
		}

		reservationsRaw, ok := resources["reservations"]
		if !ok {
			continue
		}

		reservations, ok := reservationsRaw.(map[string]any)
		if !ok {
			continue
		}

		// Remove the devices section
		delete(reservations, "devices")

		// Clean up empty parent sections
		if len(reservations) == 0 {
			delete(resources, "reservations")
		}
		if len(resources) == 0 {
			delete(deploy, "resources")
		}
		if len(deploy) == 0 {
			delete(service, "deploy")
		}
	}

	// Re-marshal the modified compose file
	return yaml.Marshal(compose)
}
