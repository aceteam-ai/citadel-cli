// Package nvr holds the reconcile logic for the `nvr` catalog module
// (aceteam-ai/citadel-cli#597): a self-hosted camera NVR built from
// docker-wyze-bridge (Wyze TUTK P2P -> RTSP) + Frigate (recording + local
// object detection).
//
// This package is the AUTHORITATIVE, unit-tested implementation of the two
// node-side reconcile concerns that a container image cannot own:
//
//  1. Generating Frigate's config.yml from the assignment config (detector,
//     retention, discovered cameras). Frigate reads /config/config.yml; the
//     module ships no static config because the detector block, retention, and
//     camera list are all assignment/runtime inputs. GenerateFrigateConfig bakes
//     in every scar observed on the live SJC build (node 1314) — see below.
//  2. Resolving + verifying the media storage target (local | nas | volume). The
//     NAS mount + fstab persistence + writability check are HOST/root operations
//     a container can't perform, and getting them wrong silently writes
//     recordings to the local disk (the #597 scar).
//
// Wiring note (documented follow-up, mirrors meeting #514's "handler wired in a
// later PR" and preempt #577's "currently INERT"): the fabric MODULE_SET
// reconcile that calls these helpers to materialize config.yml + mount the media
// target on the node is the remaining integration. It is kept out of this PR so
// the pure logic can land tested; the container-independent behavior is fully
// covered here. Secrets (wyze creds) are deliberately NOT an input to this
// package — Frigate never sees them; docker-wyze-bridge authenticates to Wyze
// via its own env and is the only container that handles credentials.
package nvr

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Detector selects Frigate's object-detection backend.
type Detector string

const (
	// DetectorOpenVINO uses the Intel iGPU via OpenVINO (device: GPU). The default
	// for the target mini node (Intel iGPU present).
	DetectorOpenVINO Detector = "openvino"
	// DetectorCPU is the fallback for hardware without OpenVINO support (e.g. an
	// AMD Vega iGPU, which has no OpenVINO runtime).
	DetectorCPU Detector = "cpu"
)

// StorageMode selects where recordings (Frigate's /media) live.
type StorageMode string

const (
	// StorageLocal points /media at a plain node path.
	StorageLocal StorageMode = "local"
	// StorageNAS mounts an NFS/SMB export and points /media at the mountpoint.
	StorageNAS StorageMode = "nas"
	// StorageVolume points /media at a citadel-provisioned volume.
	StorageVolume StorageMode = "volume"
)

// Bundled OpenVINO model shipped inside the Frigate image. Frigate 0.17 crashes
// at startup ("TypeError: stat() ... NoneType") when an openvino detector is used
// without an explicit model: block, so we always emit these paths for openvino.
const (
	OpenVINOModelPath    = "/openvino-model/ssdlite_mobilenet_v2.xml"
	OpenVINOLabelmapPath = "/openvino-model/coco_91cl_bkgr.txt"
)

// wyzeBridgeRTSPHost is the address Frigate uses to pull camera RTSP streams from
// docker-wyze-bridge. Because wyze-bridge runs with host networking (mandatory —
// TUTK P2P needs LAN broadcast + UDP hole-punching), its RTSP server binds the
// HOST. Frigate runs on the default bridge network, so it reaches the host via
// the docker host-gateway alias (compose `extra_hosts`). The port is
// wyze-bridge's default RTSP port (8554).
const (
	WyzeBridgeRTSPHostname = "host.docker.internal"
	WyzeBridgeRTSPPort     = 8554
)

// StorageSpec is the resolved media-storage configuration for one assignment.
type StorageSpec struct {
	Mode StorageMode
	// Target is a node path (local), a `host:/export` (nas), or a volume id
	// (volume). Interpreted per Mode.
	Target string
}

// Camera is one camera discovered from docker-wyze-bridge at runtime. The camera
// list is NOT part of the assignment config — wyze-bridge enumerates the Wyze
// account and exposes one RTSP stream per camera. The reconcile passes the
// discovered set here.
type Camera struct {
	// Name is the Frigate camera name (a slug; wyze-bridge uses the camera's
	// nickname lowercased with dashes).
	Name string
	// StreamPath is the RTSP path wyze-bridge serves for this camera (e.g.
	// "front-door"). Defaults to Name when empty.
	StreamPath string
}

// Config is the assignment input this package turns into a Frigate config.yml.
// It deliberately excludes wyze credentials (see package doc).
type Config struct {
	// RetentionDays bounds continuous recording. Sized by DAYS, never by free
	// space: a Synology NFS quota is invisible to `df` on the client, so a
	// free-space heuristic would never fire (the #597 scar).
	RetentionDays int
	Detector      Detector
	Storage       StorageSpec
}

// ---- Frigate config.yml shape (only the fields we set) ----

type frigateConfig struct {
	MQTT      map[string]any          `yaml:"mqtt"`
	Detectors map[string]detectorSpec `yaml:"detectors"`
	Model     *modelSpec              `yaml:"model,omitempty"`
	FFmpeg    ffmpegSpec              `yaml:"ffmpeg"`
	Record    recordSpec              `yaml:"record"`
	Cameras   map[string]cameraSpec   `yaml:"cameras"`
}

type detectorSpec struct {
	Type       string `yaml:"type"`
	Device     string `yaml:"device,omitempty"`
	NumThreads int    `yaml:"num_threads,omitempty"`
}

type modelSpec struct {
	Path             string `yaml:"path"`
	LabelmapPath     string `yaml:"labelmap_path"`
	Width            int    `yaml:"width"`
	Height           int    `yaml:"height"`
	InputTensor      string `yaml:"input_tensor"`
	InputPixelFormat string `yaml:"input_pixel_format"`
}

type ffmpegSpec struct {
	// HwaccelArgs uses Frigate's preset alias, not raw ffmpeg flags. preset-vaapi
	// drives hardware decode via /dev/dri/renderD128.
	HwaccelArgs string `yaml:"hwaccel_args"`
}

type recordSpec struct {
	Enabled    bool           `yaml:"enabled"`
	Continuous continuousSpec `yaml:"continuous"`
}

// continuousSpec writes the MIGRATED retention key. Frigate 0.17 moved
// record.retain.days -> record.continuous.days; writing the old key is silently
// ignored (recordings then use the default retention, not the assigned days).
type continuousSpec struct {
	Days int `yaml:"days"`
}

type cameraSpec struct {
	FFmpeg cameraFFmpeg `yaml:"ffmpeg"`
	Detect detectToggle `yaml:"detect"`
	// record inherits the top-level record block; no per-camera override needed.
}

type cameraFFmpeg struct {
	Inputs []cameraInput `yaml:"inputs"`
}

type cameraInput struct {
	Path  string   `yaml:"path"`
	Roles []string `yaml:"roles"`
}

type detectToggle struct {
	Enabled bool `yaml:"enabled"`
}

// GenerateFrigateConfig renders a Frigate 0.17 config.yml from the assignment
// config and the runtime-discovered camera list. It bakes in every scar from the
// live SJC build:
//
//   - openvino -> explicit model: block (bundled xml + coco labelmap) so Frigate
//     0.17 does not crash on a missing model path; cpu -> a `type: cpu` detector
//     (Frigate bundles the cpu model, so no model: block is required).
//   - ffmpeg.hwaccel_args: preset-vaapi (hardware decode).
//   - continuous recording with the MIGRATED record.continuous.days key.
//   - cameras pull RTSP from host.docker.internal:8554 (host-net wyze-bridge).
//
// It does NOT transcode: each camera records the raw H.264 wyze-bridge already
// emits (~1 Mbit/s). Retention is bounded by days only.
func GenerateFrigateConfig(cfg Config, cameras []Camera) (string, error) {
	if cfg.RetentionDays <= 0 {
		return "", fmt.Errorf("nvr: retention_days must be positive, got %d", cfg.RetentionDays)
	}

	fc := frigateConfig{
		// The reference build has no MQTT broker; Frigate makes MQTT optional only
		// when explicitly disabled.
		MQTT: map[string]any{"enabled": false},
		FFmpeg: ffmpegSpec{
			HwaccelArgs: "preset-vaapi",
		},
		Record: recordSpec{
			Enabled:    true,
			Continuous: continuousSpec{Days: cfg.RetentionDays},
		},
		Cameras: map[string]cameraSpec{},
	}

	switch cfg.Detector {
	case DetectorOpenVINO:
		fc.Detectors = map[string]detectorSpec{
			"ov": {Type: "openvino", Device: "GPU"},
		}
		fc.Model = &modelSpec{
			Path:             OpenVINOModelPath,
			LabelmapPath:     OpenVINOLabelmapPath,
			Width:            300,
			Height:           300,
			InputTensor:      "nhwc",
			InputPixelFormat: "bgr",
		}
	case DetectorCPU:
		fc.Detectors = map[string]detectorSpec{
			"cpu1": {Type: "cpu", NumThreads: 3},
		}
		// No model: block — Frigate bundles the cpu tflite model.
	default:
		return "", fmt.Errorf("nvr: unknown detector %q (want %q or %q)", cfg.Detector, DetectorOpenVINO, DetectorCPU)
	}

	for _, cam := range cameras {
		name := strings.TrimSpace(cam.Name)
		if name == "" {
			continue
		}
		streamPath := cam.StreamPath
		if strings.TrimSpace(streamPath) == "" {
			streamPath = name
		}
		fc.Cameras[name] = cameraSpec{
			FFmpeg: cameraFFmpeg{
				Inputs: []cameraInput{
					{
						Path:  fmt.Sprintf("rtsp://%s:%d/%s", WyzeBridgeRTSPHostname, WyzeBridgeRTSPPort, streamPath),
						Roles: []string{"record", "detect"},
					},
				},
			},
			Detect: detectToggle{Enabled: true},
		}
	}

	out, err := yaml.Marshal(fc)
	if err != nil {
		return "", fmt.Errorf("nvr: marshal frigate config: %w", err)
	}
	header := "# Generated by citadel nvr module (aceteam-ai/citadel-cli#597). DO NOT EDIT.\n" +
		"# Regenerated from the module assignment config on every reconcile.\n"
	return header + string(out), nil
}

// ParseCameras parses the NVR_CAMERAS env value into a camera list. The format is
// a comma-separated list of `name` or `name=stream-path` entries (stream-path
// defaults to name), e.g. "front-door,garage=garage-cam". This is the explicit,
// deterministic camera source the init container ships with; auto-discovery from
// docker-wyze-bridge's API is a runtime enhancement validated at the node-1314
// human acceptance step, not the shipped default.
func ParseCameras(raw string) []Camera {
	var out []Camera
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, stream, found := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cam := Camera{Name: name}
		if found {
			cam.StreamPath = strings.TrimSpace(stream)
		}
		out = append(out, cam)
	}
	return out
}

// CameraNames returns the sorted camera names, for stable logging/tests.
func CameraNames(cameras []Camera) []string {
	names := make([]string, 0, len(cameras))
	for _, c := range cameras {
		if n := strings.TrimSpace(c.Name); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
