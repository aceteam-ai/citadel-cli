package catalog

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// serviceRequestsGPU reports whether a single compose service (decoded into a
// generic map) requests a GPU. This is the authoritative per-service signal that
// drives the #348 GPU exemption: a GPU/inference service (vLLM, TEI, ...) must
// NOT receive the read-only / cap-drop / resource-limit hardening, because those
// defaults break it (the 2g memory default alone OOMs an inference container).
//
// Detection is fail-safe in the security direction: only a POSITIVE GPU signal
// exempts a service. An ambiguous service is treated as non-GPU and hardened.
//
// Signals, any of which mark the service as GPU:
//   - deploy.resources.reservations.devices contains an NVIDIA driver entry or a
//     "gpu" capability (the canonical Compose GPU reservation, mirrors
//     compose.StripGPUDevices' navigation).
//   - a top-level `gpus:` shorthand (e.g. `gpus: all`).
//   - `runtime: nvidia` (legacy nvidia-docker runtime selection).
func serviceRequestsGPU(svc map[string]any) bool {
	if svc == nil {
		return false
	}

	// `gpus:` shorthand (string or list form -- presence is enough).
	if _, ok := svc["gpus"]; ok {
		return true
	}

	// `runtime: nvidia`.
	if rt, ok := svc["runtime"].(string); ok && strings.EqualFold(strings.TrimSpace(rt), "nvidia") {
		return true
	}

	// deploy.resources.reservations.devices[*] (driver: nvidia or capabilities: [gpu]).
	return deployReservesGPU(svc)
}

// deployReservesGPU walks deploy.resources.reservations.devices looking for a GPU
// device entry. The navigation mirrors compose.StripGPUDevices so the two stay
// in lock-step on what counts as a GPU reservation.
func deployReservesGPU(svc map[string]any) bool {
	deploy, ok := svc["deploy"].(map[string]any)
	if !ok {
		return false
	}
	resources, ok := deploy["resources"].(map[string]any)
	if !ok {
		return false
	}
	reservations, ok := resources["reservations"].(map[string]any)
	if !ok {
		return false
	}
	devices, ok := reservations["devices"].([]any)
	if !ok {
		return false
	}
	for _, d := range devices {
		dev, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if drv, ok := dev["driver"].(string); ok && strings.EqualFold(strings.TrimSpace(drv), "nvidia") {
			return true
		}
		if caps, ok := dev["capabilities"].([]any); ok {
			for _, c := range caps {
				if s, ok := c.(string); ok && strings.EqualFold(strings.TrimSpace(s), "gpu") {
					return true
				}
			}
		}
	}
	return false
}

// decodeComposeServices decodes a compose document into a per-service generic
// map keyed by service name, preserving each service body so callers can inspect
// already-set keys (for inject-only-where-absent) and GPU signals. A document
// with no services yields an empty map and a nil error; malformed YAML returns
// the unmarshal error.
func decodeComposeServices(composeYAML string) (map[string]map[string]any, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeYAML), &doc); err != nil {
		return nil, err
	}
	if doc.Services == nil {
		return map[string]map[string]any{}, nil
	}
	return doc.Services, nil
}
