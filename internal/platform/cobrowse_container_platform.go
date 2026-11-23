package platform

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errContainerImageUnknown = errors.New("browser-runtime image is not present; platform compatibility is not yet known")

type containerImagePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

func (p containerImagePlatform) normalized() containerImagePlatform {
	p.OS = strings.ToLower(strings.TrimSpace(p.OS))
	p.Architecture = strings.ToLower(strings.TrimSpace(p.Architecture))
	p.Variant = strings.ToLower(strings.TrimSpace(p.Variant))
	switch p.Architecture {
	case "x86_64", "x86-64":
		p.Architecture = "amd64"
	case "aarch64":
		p.Architecture = "arm64"
	}
	// An omitted arm64 variant denotes the baseline v8 ISA.
	if p.Architecture == "arm64" && p.Variant == "" {
		p.Variant = "v8"
	}
	return p
}

func (p containerImagePlatform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func (p containerImagePlatform) matches(other containerImagePlatform) bool {
	p, other = p.normalized(), other.normalized()
	return p.OS != "" && p.Architecture != "" && p == other
}

// Use the engine's platform, not the CLI process's: desktop engines may run
// Linux in a VM even when the CLI is a macOS or Windows process.
func engineImagePlatform(info string, podman bool) (containerImagePlatform, error) {
	var doc struct {
		OSType       string
		Architecture string
		Variant      string
		Host         struct {
			OS, Arch, Variant string
		}
	}
	if err := json.Unmarshal([]byte(info), &doc); err != nil {
		return containerImagePlatform{}, errors.New("container engine platform is unknown")
	}
	p := containerImagePlatform{doc.OSType, doc.Architecture, doc.Variant}
	if podman {
		p = containerImagePlatform{doc.Host.OS, doc.Host.Arch, doc.Host.Variant}
	}
	p = p.normalized()
	if p.OS == "" || p.Architecture == "" {
		return p, errors.New("container engine platform is unknown")
	}
	return p, nil
}

func checkImagePlatform(metadata string, target containerImagePlatform) error {
	var image containerImagePlatform
	if err := json.Unmarshal([]byte(metadata), &image); err != nil || image.OS == "" || image.Architecture == "" {
		return errors.New("browser-runtime image platform is unknown")
	}
	if !target.matches(image) {
		return fmt.Errorf("browser-runtime image platform %s does not match engine platform %s", image.normalized(), target)
	}
	return nil
}

// For an index, select a platform-specific immutable child before pulling.
// A single manifest has no platform in its wire representation: the local
// image config must still pass checkImagePlatform after the explicit pull.
func resolveImagePlatform(image, manifest string, target containerImagePlatform) (string, error) {
	var doc struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			Digest   string                 `json:"digest"`
			Platform containerImagePlatform `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal([]byte(manifest), &doc); err != nil || doc.SchemaVersion != 2 {
		return "", errors.New("browser-runtime manifest platform is unknown")
	}
	switch doc.MediaType {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		for _, entry := range doc.Manifests {
			if !target.matches(entry.Platform) {
				continue
			}
			digest, ok := strings.CutPrefix(entry.Digest, "sha256:")
			if _, err := hex.DecodeString(digest); !ok || len(digest) != 64 || err != nil {
				return "", errors.New("browser-runtime index has an invalid child digest")
			}
			base, _, _ := strings.Cut(image, "@")
			return base + "@" + entry.Digest, nil
		}
		return "", fmt.Errorf("browser-runtime image index has no entry for engine platform %s", target)
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
		if doc.Config.Digest != "" {
			return image, nil
		}
	}
	return "", errors.New("browser-runtime manifest platform is unknown")
}
