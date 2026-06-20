package provision

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// validNameRe restricts resource names to safe characters.
var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// allowedVolumePrefixes are the only host paths allowed in volume mounts.
// This prevents containers from mounting arbitrary host directories.
var allowedVolumePrefixes = []string{
	"/tmp/",
	"/var/lib/citadel/volumes/",
}

// SetAllowedVolumePrefixes replaces the allowed volume prefixes. This is
// exposed for testing and for adding workspace directories at runtime.
func SetAllowedVolumePrefixes(prefixes []string) {
	allowedVolumePrefixes = prefixes
}

// AddAllowedVolumePrefix appends a prefix to the allowed list.
func AddAllowedVolumePrefix(prefix string) {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	allowedVolumePrefixes = append(allowedVolumePrefixes, prefix)
}

// ValidateSpec checks that a ResourceSpec is well-formed and safe.
func ValidateSpec(spec *ResourceSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !validNameRe.MatchString(spec.Name) {
		return fmt.Errorf("invalid name %q: must match %s", spec.Name, validNameRe.String())
	}

	switch spec.Type {
	case ResourceTypeDocker:
		// OK
	case ResourceTypeLXC, ResourceTypeVM:
		return fmt.Errorf("resource type %q is not yet supported", spec.Type)
	default:
		return fmt.Errorf("unknown resource type %q", spec.Type)
	}

	if spec.Image == "" {
		return fmt.Errorf("image is required for %s resources", spec.Type)
	}

	for _, v := range spec.Volumes {
		if err := validateVolumePath(v.HostPath); err != nil {
			return fmt.Errorf("volume %q: %w", v.HostPath, err)
		}
		if v.ContainerPath == "" {
			return fmt.Errorf("volume %q: container_path is required", v.HostPath)
		}
	}

	for _, p := range spec.Ports {
		if p.HostPort < 1 || p.HostPort > 65535 {
			return fmt.Errorf("invalid host port %d: must be 1-65535", p.HostPort)
		}
		if p.ContainerPort < 1 || p.ContainerPort > 65535 {
			return fmt.Errorf("invalid container port %d: must be 1-65535", p.ContainerPort)
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return fmt.Errorf("invalid protocol %q: must be tcp or udp", proto)
		}
	}

	return nil
}

func validateVolumePath(hostPath string) error {
	if hostPath == "" {
		return fmt.Errorf("host_path is required")
	}
	if !filepath.IsAbs(hostPath) {
		return fmt.Errorf("host_path must be absolute, got %q", hostPath)
	}
	cleaned := filepath.Clean(hostPath)
	if cleaned != hostPath && cleaned+"/" != hostPath {
		return fmt.Errorf("host_path contains path traversal")
	}
	pathWithSlash := cleaned + "/"
	for _, prefix := range allowedVolumePrefixes {
		if strings.HasPrefix(pathWithSlash, prefix) || cleaned == strings.TrimSuffix(prefix, "/") {
			return nil
		}
	}
	return fmt.Errorf("host_path %q is not under an allowed prefix (%s)",
		hostPath, strings.Join(allowedVolumePrefixes, ", "))
}
