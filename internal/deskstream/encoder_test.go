package deskstream

import (
	"strings"
	"testing"
)

func TestSelectEncoder_PrefersHardware(t *testing.T) {
	// All available: VA-API wins.
	all := func(string) bool { return true }
	enc, err := selectEncoder(all)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enc != EncoderVAAPI {
		t.Errorf("got %s, want VA-API (first preference)", enc)
	}
}

func TestSelectEncoder_FallsBackToNVENC(t *testing.T) {
	only := func(name string) bool { return name == string(EncoderNVENC) || name == string(EncoderSoftware) }
	enc, err := selectEncoder(only)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enc != EncoderNVENC {
		t.Errorf("got %s, want NVENC", enc)
	}
}

func TestSelectEncoder_FallsBackToSoftware(t *testing.T) {
	only := func(name string) bool { return name == string(EncoderSoftware) }
	enc, err := selectEncoder(only)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enc != EncoderSoftware {
		t.Errorf("got %s, want libx264", enc)
	}
}

func TestSelectEncoder_NoneAvailable(t *testing.T) {
	none := func(string) bool { return false }
	if _, err := selectEncoder(none); err == nil {
		t.Error("expected an error when no encoder is available")
	}
}

func TestBuildFFmpegArgs_Common(t *testing.T) {
	cfg := EncodeConfig{Display: ":0", Width: 1920, Height: 1080, FPS: 30, KeyframeInterval: 60}
	args := buildFFmpegArgs(cfg, EncoderSoftware)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-f x11grab",
		"-framerate 30",
		"-video_size 1920x1080",
		"-i :0",
		"-c:v libx264",
		"-tune zerolatency",
		"-g 60",
		"-force_key_frames",
		"-f h264",
		"pipe:1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("software args missing %q\nargs: %s", want, joined)
		}
	}
}

func TestBuildFFmpegArgs_DefaultKeyframeIsTwoSeconds(t *testing.T) {
	// With FPS=15 and no explicit interval, GOP should default to fps*2 = 30.
	cfg := EncodeConfig{Display: ":0", Width: 800, Height: 600, FPS: 15}
	args := buildFFmpegArgs(cfg, EncoderSoftware)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-g 30") {
		t.Errorf("expected default GOP of fps*2=30, got: %s", joined)
	}
}

func TestBuildFFmpegArgs_VAAPI(t *testing.T) {
	cfg := EncodeConfig{Display: ":0", Width: 1280, Height: 720, FPS: 15, KeyframeInterval: 30}
	args := buildFFmpegArgs(cfg, EncoderVAAPI)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-vaapi_device", "format=nv12,hwupload", "-c:v h264_vaapi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("VA-API args missing %q\nargs: %s", want, joined)
		}
	}
}

func TestBuildFFmpegArgs_NVENC(t *testing.T) {
	cfg := EncodeConfig{Display: ":0", Width: 1280, Height: 720, FPS: 15, KeyframeInterval: 30}
	args := buildFFmpegArgs(cfg, EncoderNVENC)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c:v h264_nvenc", "-tune ll"} {
		if !strings.Contains(joined, want) {
			t.Errorf("NVENC args missing %q\nargs: %s", want, joined)
		}
	}
}
