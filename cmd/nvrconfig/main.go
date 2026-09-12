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
// (comma-separated `name` / `name=stream`), NVR_SEMANTIC_SEARCH (bool, default
// false) and NVR_TRACK_OBJECTS (comma-separated labels, default `person`) — #1039.
// Wyze credentials are NOT read here — only docker-wyze-bridge sees them.
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

	// Semantic search is OFF by default (#1039). Parse strictly and fail loud on a
	// malformed value (the RETENTION_DAYS pattern), per this command's contract of
	// exiting non-zero on any bad input — do not silently treat garbage as false.
	semanticSearch := false
	if raw := strings.TrimSpace(os.Getenv("NVR_SEMANTIC_SEARCH")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("NVR_SEMANTIC_SEARCH %q is not a boolean (true/false)", raw)
		}
		semanticSearch = v
	}

	// Tracked objects default to `person` (Frigate's own default). Widening this is
	// what makes a dog/cat/package detectable and, with semantic search, searchable.
	trackObjects := nvr.ParseTrackObjects(getenvDefault("NVR_TRACK_OBJECTS", nvr.DefaultTrackObject))

	// Semantic search embeds thumbnails of TRACKED objects only, so it is
	// near-useless when only `person` is tracked. Warn loudly rather than silently
	// widening the track list: more tracked objects = more detection + embedding
	// work, which is costly on a detector-less (CPU-only) node.
	if semanticSearch && len(trackObjects) == 1 && trackObjects[0] == nvr.DefaultTrackObject {
		fmt.Fprintln(os.Stderr, "nvrconfig: WARNING NVR_SEMANTIC_SEARCH is on but only 'person' is tracked; "+
			"semantic search embeds thumbnails of TRACKED objects only. Set NVR_TRACK_OBJECTS "+
			"(e.g. \"person,dog,cat\") to make other objects searchable.")
	}

	// MQTT is on by default: the module ships a node-local broker, and Frigate
	// has no other real-time event egress (#637). NVR_MQTT=false opts out.
	mqttEnabled := strings.ToLower(strings.TrimSpace(getenvDefault("NVR_MQTT", "true"))) != "false"
	mqttPort := nvr.DefaultMQTTPort
	if raw := strings.TrimSpace(os.Getenv("NVR_MQTT_PORT")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			mqttPort = v
		}
	}

	cfg := nvr.Config{
		RetentionDays:  retention,
		Detector:       detector,
		Storage:        nvr.StorageSpec{Mode: mode, Target: os.Getenv("NVR_STORAGE_TARGET")},
		SemanticSearch: semanticSearch,
		TrackObjects:   trackObjects,
		MQTT: nvr.MQTTSpec{
			Enabled:     mqttEnabled,
			Host:        getenvDefault("NVR_MQTT_HOST", "mosquitto"),
			Port:        mqttPort,
			User:        os.Getenv("NVR_MQTT_USER"),
			Password:    os.Getenv("NVR_MQTT_PASSWORD"),
			TopicPrefix: getenvDefault("NVR_MQTT_TOPIC_PREFIX", nvr.DefaultMQTTTopicPrefix),
		},
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
	fmt.Printf("nvrconfig: wrote %s (detector=%s retention=%dd storage=%s semantic_search=%t track=%v cameras=%v)\n",
		configPath, detector, retention, mode, semanticSearch, trackObjects, nvr.CameraNames(cameras))
	return nil
}

func getenvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
