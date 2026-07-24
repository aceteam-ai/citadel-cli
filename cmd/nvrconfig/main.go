// Command nvrconfig is the init-container entrypoint for the nvr catalog module
// (aceteam-ai/citadel-cli#597). It runs to completion BEFORE Frigate starts
// (compose `depends_on: condition: service_completed_successfully`) and does the
// two runtime jobs a stock Frigate image cannot:
//
//  1. Generate /config/config.yml from the assignment env vars + the camera list,
//     using the SAME tested generator the rest of citadel uses
//     (internal/nvr.GenerateFrigateConfig) — one implementation, compiled in, no
//     drift with a parallel script.
//  2. For storage.mode=nas, VERIFY /media is genuinely a network filesystem
//     (statfs magic) and root-writable BEFORE Frigate writes a single recording —
//     the shipped guard against the #1 storage scar (a failed NFS mount silently
//     writing to the local disk). Inside this container /media is a bind mount, so
//     this MUST check the filesystem type, not mountedness.
//
// It exits non-zero (failing the whole stack, loudly and actionably) on any bad
// input — including zero cameras, which Frigate 0.17 otherwise turns into an
// opaque crash-loop.
//
// Env inputs (from the module .env / assignment): NVR_DETECTOR (openvino|cpu),
// NVR_RETENTION_DAYS, NVR_STORAGE_MODE (local|nas|volume), NVR_CAMERAS
// (comma-separated `name` / `name=stream`). Wyze credentials are NOT read here —
// only docker-wyze-bridge sees them.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nvr"
)

const (
	configPath = "/config/config.yml"
	mediaPath  = "/media"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nvrconfig: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	detector := nvr.Detector(strings.ToLower(strings.TrimSpace(getenvDefault("NVR_DETECTOR", "openvino"))))

	retention := 12
	if raw := strings.TrimSpace(os.Getenv("NVR_RETENTION_DAYS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("NVR_RETENTION_DAYS %q is not an integer", raw)
		}
		retention = n
	}

	mode := nvr.StorageMode(strings.ToLower(strings.TrimSpace(getenvDefault("NVR_STORAGE_MODE", "local"))))

	cameras := nvr.ParseCameras(os.Getenv("NVR_CAMERAS"))
	if len(cameras) == 0 {
		return fmt.Errorf("no cameras: set NVR_CAMERAS to a comma-separated list of camera stream names " +
			"(e.g. \"front-door,garage=garage-cam\"). Frigate 0.17 refuses to start with an empty cameras: block")
	}

	// nas: refuse to start unless /media is a real network mount (root-writable).
	// This is the shipped guard against silently recording to the local disk.
	if mode == nvr.StorageNAS {
		if err := nvr.VerifyMediaIsNetworkFS(mediaPath, os.Getuid(), nvr.DefaultNetFSProbe()); err != nil {
			return err
		}
	}

	cfg := nvr.Config{
		RetentionDays: retention,
		Detector:      detector,
		Storage:       nvr.StorageSpec{Mode: mode, Target: os.Getenv("NVR_STORAGE_TARGET")},
	}
	yamlOut, err := nvr.GenerateFrigateConfig(cfg, cameras)
	if err != nil {
		return err
	}

	if err := os.MkdirAll("/config", 0o755); err != nil {
		return fmt.Errorf("create /config: %w", err)
	}
	if err := os.WriteFile(configPath, []byte(yamlOut), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	fmt.Printf("nvrconfig: wrote %s (detector=%s retention=%dd storage=%s cameras=%v)\n",
		configPath, detector, retention, mode, nvr.CameraNames(cameras))
	return nil
}

func getenvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
