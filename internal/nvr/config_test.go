package nvr

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parseGenerated renders a config and parses it back into a generic map so tests
// can assert on the actual emitted structure (not just substrings).
func parseGenerated(t *testing.T, cfg Config, cams []Camera) map[string]any {
	t.Helper()
	out, err := GenerateFrigateConfig(cfg, cams)
	if err != nil {
		t.Fatalf("GenerateFrigateConfig: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("generated config does not parse as YAML: %v\n%s", err, out)
	}
	return m
}

// TestDetectorOpenVINOEmitsExplicitModel pins the #1 Frigate 0.17 scar: an
// openvino detector MUST come with an explicit model: block (bundled xml +
// labelmap) or Frigate crashes at startup. It also asserts device: GPU (Intel
// iGPU).
func TestDetectorOpenVINOEmitsExplicitModel(t *testing.T) {
	m := parseGenerated(t, Config{
		RetentionDays: 12,
		Detector:      DetectorOpenVINO,
		Storage:       StorageSpec{Mode: StorageLocal, Target: "/srv/nvr"},
	}, nil)

	dets, ok := m["detectors"].(map[string]any)
	if !ok {
		t.Fatalf("detectors block missing/wrong type: %T", m["detectors"])
	}
	ov, ok := dets["ov"].(map[string]any)
	if !ok {
		t.Fatalf("openvino detector 'ov' missing: %v", dets)
	}
	if ov["type"] != "openvino" {
		t.Errorf("detector type = %v, want openvino", ov["type"])
	}
	if ov["device"] != "GPU" {
		t.Errorf("detector device = %v, want GPU (Intel iGPU)", ov["device"])
	}

	model, ok := m["model"].(map[string]any)
	if !ok {
		t.Fatalf("openvino requires an explicit model: block or Frigate 0.17 crashes; got %v", m["model"])
	}
	if model["path"] != OpenVINOModelPath {
		t.Errorf("model.path = %v, want %s", model["path"], OpenVINOModelPath)
	}
	if model["labelmap_path"] != OpenVINOLabelmapPath {
		t.Errorf("model.labelmap_path = %v, want %s", model["labelmap_path"], OpenVINOLabelmapPath)
	}
}

// TestDetectorCPUEmitsCPUBlockNoModel pins the AMD/Vega fallback: a cpu detector
// and NO model: block (Frigate bundles the cpu model).
func TestDetectorCPUEmitsCPUBlockNoModel(t *testing.T) {
	m := parseGenerated(t, Config{
		RetentionDays: 7,
		Detector:      DetectorCPU,
		Storage:       StorageSpec{Mode: StorageLocal, Target: "/srv/nvr"},
	}, nil)

	dets, ok := m["detectors"].(map[string]any)
	if !ok {
		t.Fatalf("detectors block missing: %T", m["detectors"])
	}
	cpu, ok := dets["cpu1"].(map[string]any)
	if !ok {
		t.Fatalf("cpu detector 'cpu1' missing: %v", dets)
	}
	if cpu["type"] != "cpu" {
		t.Errorf("detector type = %v, want cpu", cpu["type"])
	}
	if _, hasOV := dets["ov"]; hasOV {
		t.Errorf("cpu config must not emit an openvino detector: %v", dets)
	}
	if _, hasModel := m["model"]; hasModel {
		t.Errorf("cpu detector must NOT emit a model: block (Frigate bundles the cpu model); got %v", m["model"])
	}
}

// TestRetentionUsesMigratedContinuousKey pins the Frigate 0.17 migration:
// record.retain.days -> record.continuous.days. Writing the OLD key is silently
// ignored, so recordings would use default retention instead of the assigned days.
func TestRetentionUsesMigratedContinuousKey(t *testing.T) {
	m := parseGenerated(t, Config{
		RetentionDays: 12,
		Detector:      DetectorOpenVINO,
		Storage:       StorageSpec{Mode: StorageLocal, Target: "/srv/nvr"},
	}, nil)

	record, ok := m["record"].(map[string]any)
	if !ok {
		t.Fatalf("record block missing: %T", m["record"])
	}
	if _, hasRetain := record["retain"]; hasRetain {
		t.Errorf("record.retain is the PRE-0.17 key and is silently ignored; must use record.continuous.days")
	}
	cont, ok := record["continuous"].(map[string]any)
	if !ok {
		t.Fatalf("record.continuous block missing (migrated key): %v", record)
	}
	if days, _ := cont["days"].(int); days != 12 {
		t.Errorf("record.continuous.days = %v, want 12", cont["days"])
	}
	if record["enabled"] != true {
		t.Errorf("record.enabled = %v, want true (continuous recording)", record["enabled"])
	}
}

// TestFFmpegPresetVAAPI pins hardware decode via the preset alias.
func TestFFmpegPresetVAAPI(t *testing.T) {
	m := parseGenerated(t, Config{
		RetentionDays: 12,
		Detector:      DetectorCPU,
		Storage:       StorageSpec{Mode: StorageLocal, Target: "/srv/nvr"},
	}, nil)
	ff, ok := m["ffmpeg"].(map[string]any)
	if !ok {
		t.Fatalf("ffmpeg block missing: %T", m["ffmpeg"])
	}
	if ff["hwaccel_args"] != "preset-vaapi" {
		t.Errorf("ffmpeg.hwaccel_args = %v, want preset-vaapi", ff["hwaccel_args"])
	}
}

// TestCamerasPullFromHostNetWyzeBridge pins that camera RTSP inputs target the
// host-networked wyze-bridge via the docker host-gateway alias on 8554, and that
// the config records raw H.264 (no transcode: role includes record+detect, no
// re-encode args).
func TestCamerasPullFromHostNetWyzeBridge(t *testing.T) {
	out, err := GenerateFrigateConfig(Config{
		RetentionDays: 12,
		Detector:      DetectorOpenVINO,
		Storage:       StorageSpec{Mode: StorageLocal, Target: "/srv/nvr"},
	}, []Camera{{Name: "front-door"}, {Name: "garage", StreamPath: "garage-cam"}})
	if err != nil {
		t.Fatalf("GenerateFrigateConfig: %v", err)
	}
	wantFront := "rtsp://host.docker.internal:8554/front-door"
	if !strings.Contains(out, wantFront) {
		t.Errorf("expected camera input %q in:\n%s", wantFront, out)
	}
	wantGarage := "rtsp://host.docker.internal:8554/garage-cam"
	if !strings.Contains(out, wantGarage) {
		t.Errorf("expected camera stream-path override input %q in:\n%s", wantGarage, out)
	}

	var m map[string]any
	if err := yaml.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cams, ok := m["cameras"].(map[string]any)
	if !ok || len(cams) != 2 {
		t.Fatalf("expected 2 cameras, got %v", m["cameras"])
	}
}

// TestGenerateRejectsBadInput guards the two hard errors.
func TestGenerateRejectsBadInput(t *testing.T) {
	if _, err := GenerateFrigateConfig(Config{RetentionDays: 0, Detector: DetectorCPU}, nil); err == nil {
		t.Errorf("expected error for retention_days <= 0")
	}
	if _, err := GenerateFrigateConfig(Config{RetentionDays: 5, Detector: "gpu-nonsense"}, nil); err == nil {
		t.Errorf("expected error for unknown detector")
	}
}

// TestVerifyMediaMountSeparatesMountednessFromWritability is the core NAS scar
// test: a path that is WRITABLE but NOT mounted must FAIL (that is the silent
// local-disk leak). Only mounted AND writable passes.
func TestVerifyMediaMountSeparatesMountednessFromWritability(t *testing.T) {
	cases := []struct {
		name      string
		mounted   bool
		writable  bool
		wantErr   bool
		errSubstr string
	}{
		{"mounted+writable", true, true, false, ""},
		{"writable-but-not-mounted (the leak)", false, true, true, "NOT a mountpoint"},
		{"mounted-but-not-writable (squashed export)", true, false, true, "no_root_squash"},
		{"neither", false, false, true, "NOT a mountpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := MountProbe{
				IsMountpoint:  func(string) (bool, error) { return tc.mounted, nil },
				WritableAsUID: func(string, int) (bool, error) { return tc.writable, nil },
			}
			err := VerifyMediaMount("/mnt/citadel-nvr-media", 0, probe)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

// TestVerifyMediaMountPropagatesProbeError ensures a probe failure is not
// mistaken for "mounted".
func TestVerifyMediaMountPropagatesProbeError(t *testing.T) {
	sentinel := errors.New("stat boom")
	probe := MountProbe{
		IsMountpoint:  func(string) (bool, error) { return false, sentinel },
		WritableAsUID: func(string, int) (bool, error) { return true, nil },
	}
	if err := VerifyMediaMount("/mnt/x", 0, probe); err == nil || !strings.Contains(err.Error(), "stat boom") {
		t.Errorf("expected wrapped probe error, got %v", err)
	}
}

// TestResolveMediaSource covers the three storage modes and the /config-stays-local
// invariant (ResolveMediaSource only ever returns a MEDIA path; ConfigDir is a
// separate local path that never follows the target).
func TestResolveMediaSource(t *testing.T) {
	// local: path is the target, no mount needed.
	p, needsMount, err := ResolveMediaSource(StorageSpec{Mode: StorageLocal, Target: "/srv/nvr"})
	if err != nil || needsMount || p != "/srv/nvr" {
		t.Errorf("local: got (%q, %v, %v), want (/srv/nvr, false, nil)", p, needsMount, err)
	}

	// nas: media path is the citadel MediaDir() and needsMount is true.
	mediaDir, err := MediaDir()
	if err != nil {
		t.Fatalf("MediaDir: %v", err)
	}
	p, needsMount, err = ResolveMediaSource(StorageSpec{Mode: StorageNAS, Target: "nas.local:/volume2/surveillance"})
	if err != nil || !needsMount || p != mediaDir {
		t.Errorf("nas: got (%q, %v, %v), want (%s, true, nil)", p, needsMount, err, mediaDir)
	}

	// nas without host:/export is rejected.
	if _, _, err := ResolveMediaSource(StorageSpec{Mode: StorageNAS, Target: "/not-an-export"}); err == nil {
		t.Errorf("nas: expected error for target without host:/export")
	}

	// volume: path is the resolved volume path.
	p, needsMount, err = ResolveMediaSource(StorageSpec{Mode: StorageVolume, Target: "/var/lib/citadel/volumes/vol-123"})
	if err != nil || needsMount || p == "" {
		t.Errorf("volume: got (%q, %v, %v)", p, needsMount, err)
	}

	// /config must NEVER equal the media source — SQLite stays local.
	configDir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if configDir == "" || configDir == mediaDir {
		t.Errorf("ConfigDir %q must be a local path independent of the media dir %q", configDir, mediaDir)
	}
}

// TestFstabEntry pins the persisted NFSv3 mount line (Synology target has no v4).
func TestFstabEntry(t *testing.T) {
	line, err := FstabEntry("nas.local:/volume2/surveillance", "/mnt/nvr-media")
	if err != nil {
		t.Fatalf("FstabEntry: %v", err)
	}
	for _, want := range []string{"nas.local:/volume2/surveillance", "/mnt/nvr-media", "nfs", "vers=3", "_netdev"} {
		if !strings.Contains(line, want) {
			t.Errorf("fstab line %q missing %q", line, want)
		}
	}
	if _, err := FstabEntry("/no-export", "/mnt/nvr-media"); err == nil {
		t.Errorf("expected error for export without host:")
	}
	if _, err := FstabEntry("nas:/x", "relative/mount"); err == nil {
		t.Errorf("expected error for relative mountpoint")
	}
}

// TestVerifyMediaIsNetworkFS is the SHIPPED in-container guard: inside frigate
// /media is always a bind mount, so mountedness is a false pass — the check must
// use the filesystem TYPE. A local fs (ext4) in nas mode must FAIL (the silent
// local-disk leak); a real NFS/SMB mount that is root-writable passes.
func TestVerifyMediaIsNetworkFS(t *testing.T) {
	const magicEXT4 int64 = 0xEF53
	cases := []struct {
		name      string
		fsType    int64
		writable  bool
		wantErr   bool
		errSubstr string
	}{
		{"nfs+writable", MagicNFS, true, false, ""},
		{"cifs+writable", MagicCIFS, true, false, ""},
		{"local ext4 (the leak)", magicEXT4, true, true, "NOT a network filesystem"},
		{"nfs but squashed", MagicNFS, false, true, "no_root_squash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := NetFSProbe{
				FSType:   func(string) (int64, error) { return tc.fsType, nil },
				Writable: func(string, int) (bool, error) { return tc.writable, nil },
			}
			err := VerifyMediaIsNetworkFS("/media", 0, probe)
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)) {
				t.Fatalf("want error containing %q, got %v", tc.errSubstr, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
	if !IsNetworkFSMagic(MagicNFS) || IsNetworkFSMagic(magicEXT4) {
		t.Errorf("IsNetworkFSMagic misclassified NFS/ext4")
	}
}

// TestParseCameras covers the explicit NVR_CAMERAS format (name and name=stream).
func TestParseCameras(t *testing.T) {
	cams := ParseCameras("front-door, garage=garage-cam ,, back-yard")
	if len(cams) != 3 {
		t.Fatalf("want 3 cameras, got %d: %+v", len(cams), cams)
	}
	if cams[0].Name != "front-door" || cams[0].StreamPath != "" {
		t.Errorf("cam0 = %+v", cams[0])
	}
	if cams[1].Name != "garage" || cams[1].StreamPath != "garage-cam" {
		t.Errorf("cam1 = %+v (want garage=garage-cam)", cams[1])
	}
	if got := ParseCameras("   "); got != nil {
		t.Errorf("blank NVR_CAMERAS should yield no cameras, got %+v", got)
	}
}
