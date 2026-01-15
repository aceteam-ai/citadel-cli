// Package compose provides utilities for modifying Docker Compose files.
package compose

import (
	"gopkg.in/yaml.v3"
)

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
