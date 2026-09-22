// Package compose provides utilities for modifying Docker Compose files.
package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const nvidiaCDIPrefix = "nvidia.com/gpu="

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

// RewriteGPUDevicesForPodman translates the Docker Compose GPU spellings
// Citadel accepts into Podman's native NVIDIA CDI device form. It is pure so
// callers can materialize a temporary compose file without changing the
// installed source file.
//
// The accepted input forms mirror the GPU detection in internal/catalog:
//
//   - deploy.resources.reservations.devices entries with driver: nvidia or a
//     gpu capability
//   - the top-level gpus shorthand
//   - the legacy runtime: nvidia spelling
//
// Non-GPU device reservations and existing service-level devices are
// preserved. An ambiguous GPU selector fails closed instead of deleting the
// Docker reservation and accidentally starting a CPU-only container.
func RewriteGPUDevicesForPodman(content []byte) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	servicesRaw, ok := doc["services"]
	if !ok {
		return content, nil
	}
	services, ok := servicesRaw.(map[string]any)
	if !ok {
		return content, nil
	}

	changed := false
	for name, raw := range services {
		svc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		selectors, serviceChanged, err := podmanGPUSelectors(svc)
		if err != nil {
			return nil, fmt.Errorf("service %s GPU reservation: %w", name, err)
		}
		if !serviceChanged {
			continue
		}
		changed = true
		devices, err := appendCDIDevices(svc["devices"], selectors)
		if err != nil {
			return nil, fmt.Errorf("service %s devices: %w", name, err)
		}
		svc["devices"] = devices
	}
	if !changed {
		return content, nil
	}
	return yaml.Marshal(doc)
}

// MaterializePodmanGPUCompose rewrites path into a private temporary compose
// file when it contains a GPU request. The installed compose file is never
// changed. cleanup is always safe to call.
func MaterializePodmanGPUCompose(path string) (actualPath string, cleanup func(), err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", func() {}, err
	}
	rewritten, err := RewriteGPUDevicesForPodman(content)
	if err != nil {
		return "", func() {}, err
	}
	if string(rewritten) == string(content) {
		return path, func() {}, nil
	}
	// Keep the copy beside the source: Compose resolves relative build contexts,
	// bind mounts, and env files from the project directory. A /tmp copy would
	// silently retarget those paths.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".citadel-podman-*.yml")
	if err != nil {
		return "", func() {}, err
	}
	tmpPath := tmp.Name()
	cleanup = func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", func() {}, err
	}
	if _, err := tmp.Write(rewritten); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmpPath, cleanup, nil
}

func podmanGPUSelectors(svc map[string]any) ([]string, bool, error) {
	seen := map[string]bool{}
	var selected []string
	add := func(selectors []string) {
		for _, selector := range selectors {
			if !seen[selector] {
				seen[selector] = true
				selected = append(selected, selector)
			}
		}
	}
	changed := false

	if raw, ok := svc["gpus"]; ok {
		selectors, err := gpuSelectors(raw)
		if err != nil {
			return nil, false, fmt.Errorf("gpus: %w", err)
		}
		add(selectors)
		delete(svc, "gpus")
		changed = true
	}

	if runtimeName, ok := svc["runtime"].(string); ok && strings.EqualFold(strings.TrimSpace(runtimeName), "nvidia") {
		add([]string{"all"})
		delete(svc, "runtime")
		changed = true
	}

	deploy, _ := svc["deploy"].(map[string]any)
	resources, _ := deploy["resources"].(map[string]any)
	reservations, _ := resources["reservations"].(map[string]any)
	devices, _ := reservations["devices"].([]any)
	if len(devices) > 0 {
		kept := make([]any, 0, len(devices))
		for _, raw := range devices {
			device, ok := raw.(map[string]any)
			if !ok || !composeDeviceRequestsGPU(device) {
				kept = append(kept, raw)
				continue
			}
			selectors, err := reservationSelectors(device)
			if err != nil {
				return nil, false, err
			}
			add(selectors)
			changed = true
		}
		if changed {
			if len(kept) == 0 {
				delete(reservations, "devices")
			} else {
				reservations["devices"] = kept
			}
			cleanEmptyDeploySections(svc, deploy, resources, reservations)
		}
	}

	return selected, changed, nil
}

func composeDeviceRequestsGPU(device map[string]any) bool {
	if driver, ok := device["driver"].(string); ok && strings.EqualFold(strings.TrimSpace(driver), "nvidia") {
		return true
	}
	capabilities, _ := device["capabilities"].([]any)
	for _, raw := range capabilities {
		if capability, ok := raw.(string); ok && strings.EqualFold(strings.TrimSpace(capability), "gpu") {
			return true
		}
	}
	return false
}

func reservationSelectors(device map[string]any) ([]string, error) {
	if ids, ok := device["device_ids"].([]any); ok {
		selectors := make([]string, 0, len(ids))
		for _, raw := range ids {
			id, ok := raw.(string)
			if !ok || !validCDISelector(id) {
				return nil, fmt.Errorf("invalid device_id %v", raw)
			}
			selectors = append(selectors, strings.TrimSpace(id))
		}
		if len(selectors) == 0 {
			return nil, fmt.Errorf("device_ids is empty")
		}
		return selectors, nil
	}
	if count, ok := device["count"]; ok {
		return gpuSelectors(count)
	}
	return nil, fmt.Errorf("GPU reservation has neither count nor device_ids")
}

func gpuSelectors(raw any) ([]string, error) {
	switch value := raw.(type) {
	case string:
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "all") {
			return []string{"all"}, nil
		}
		count, err := strconv.Atoi(value)
		if err == nil {
			return gpuCountSelectors(count)
		}
		if validCDISelector(value) {
			return []string{value}, nil
		}
		return nil, fmt.Errorf("unsupported selector %q", value)
	case int:
		return gpuCountSelectors(value)
	case int64:
		return gpuCountSelectors(int(value))
	case uint64:
		if value > uint64(^uint(0)>>1) {
			return nil, fmt.Errorf("GPU count %d is too large", value)
		}
		return gpuCountSelectors(int(value))
	case []any:
		selectors := make([]string, 0, len(value))
		for _, item := range value {
			itemSelectors, err := gpuSelectors(item)
			if err != nil {
				return nil, err
			}
			selectors = append(selectors, itemSelectors...)
		}
		if len(selectors) == 0 {
			return nil, fmt.Errorf("GPU selector list is empty")
		}
		return selectors, nil
	case map[string]any:
		return reservationSelectors(value)
	default:
		return nil, fmt.Errorf("unsupported selector type %T", raw)
	}
}

func gpuCountSelectors(count int) ([]string, error) {
	if count <= 0 {
		return nil, fmt.Errorf("GPU count must be positive")
	}
	selectors := make([]string, count)
	for i := range selectors {
		selectors[i] = strconv.Itoa(i)
	}
	return selectors, nil
}

func validCDISelector(selector string) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" || strings.ContainsAny(selector, " ,\t\r\n") {
		return false
	}
	for _, r := range selector {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r) {
			continue
		}
		return false
	}
	return true
}

func appendCDIDevices(raw any, selectors []string) ([]any, error) {
	var devices []any
	if raw != nil {
		var ok bool
		devices, ok = raw.([]any)
		if !ok {
			return nil, fmt.Errorf("expected a list, got %T", raw)
		}
	}
	existing := map[string]bool{}
	for _, device := range devices {
		if value, ok := device.(string); ok {
			existing[value] = true
		}
	}
	for _, selector := range selectors {
		device := nvidiaCDIPrefix + selector
		if !existing[device] {
			devices = append(devices, device)
			existing[device] = true
		}
	}
	return devices, nil
}

func cleanEmptyDeploySections(svc, deploy, resources, reservations map[string]any) {
	if len(reservations) == 0 {
		delete(resources, "reservations")
	}
	if len(resources) == 0 {
		delete(deploy, "resources")
	}
	if len(deploy) == 0 {
		delete(svc, "deploy")
	}
}

// RequiredLimitControllers returns the cgroup v2 controllers needed to honor
// resource-limit keys in one or more compose documents. Unknown keys do not
// invent requirements. The result is ordered cpu, memory, pids.
func RequiredLimitControllers(contents ...[]byte) ([]string, error) {
	required := map[string]bool{}
	for _, content := range contents {
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return nil, err
		}
		for _, svc := range doc.Services {
			markServiceLimitControllers(svc, required)
		}
	}
	ordered := make([]string, 0, 3)
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if required[controller] {
			ordered = append(ordered, controller)
		}
	}
	return ordered, nil
}

// RequiredLimitControllersFromFiles is the filesystem adapter for
// RequiredLimitControllers. Empty paths are ignored.
func RequiredLimitControllersFromFiles(paths ...string) ([]string, error) {
	contents := make([][]byte, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, err
		}
		contents = append(contents, content)
	}
	return RequiredLimitControllers(contents...)
}

func markServiceLimitControllers(svc map[string]any, required map[string]bool) {
	for _, key := range []string{"cpus", "cpu_count", "cpu_percent", "cpu_period", "cpu_quota", "cpu_shares", "cpuset"} {
		if _, ok := svc[key]; ok {
			required["cpu"] = true
		}
	}
	for _, key := range []string{"mem_limit", "mem_reservation", "memswap_limit", "mem_swappiness"} {
		if _, ok := svc[key]; ok {
			required["memory"] = true
		}
	}
	if _, ok := svc["pids_limit"]; ok {
		required["pids"] = true
	}
	deploy, _ := svc["deploy"].(map[string]any)
	resources, _ := deploy["resources"].(map[string]any)
	limits, _ := resources["limits"].(map[string]any)
	if _, ok := limits["cpus"]; ok {
		required["cpu"] = true
	}
	if _, ok := limits["memory"]; ok {
		required["memory"] = true
	}
	if _, ok := limits["pids"]; ok {
		required["pids"] = true
	}
}
