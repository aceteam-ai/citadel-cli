package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	audioMixBuiltinName = "audio-mix"
	audioMixOutput      = "mix.wav"
	audioMixTargetLUFS  = -14
	audioMixTruePeakDB  = -2
	audioMixLoudnessLRA = 11
	audioMixMaxInputs   = 16
)

type audioMixDeps struct {
	lookFFmpeg func() (string, error)
	run        func(context.Context, string, []string) ([]byte, error)
}

func init() {
	RegisterBuiltinTemplateRunner(audioMixBuiltinName, func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) ([]string, error) {
		return runAudioMixBuiltin(ctx, params, inputs, outDir, liveAudioMixDeps())
	})
}

func liveAudioMixDeps() audioMixDeps {
	return audioMixDeps{
		lookFFmpeg: func() (string, error) { return exec.LookPath("ffmpeg") },
		run: func(ctx context.Context, bin string, args []string) ([]byte, error) {
			return exec.CommandContext(ctx, bin, args...).CombinedOutput()
		},
	}
}

// runAudioMixBuiltin mixes already-produced stems, then applies ffmpeg's
// measured two-pass EBU R128 normalization. Paper Trail's procedural synthesis
// stays in paper-trail; this runner owns only the portable mix/normalization
// bridge requested by #1161.
func runAudioMixBuiltin(ctx context.Context, params json.RawMessage, inputs []string, outDir string, deps audioMixDeps) ([]string, error) {
	if _, err := decodeBuiltinParams(params); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("audio-mix requires at least one input stem")
	}
	if len(inputs) > audioMixMaxInputs {
		return nil, fmt.Errorf("audio-mix accepts at most %d input stems, got %d", audioMixMaxInputs, len(inputs))
	}
	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("audio input %q is not a regular file", input)
		}
	}
	ffmpeg, err := deps.lookFFmpeg()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: install ffmpeg on the render node")
	}

	analysisArgs := audioMixAnalysisArgs(inputs)
	analysis, err := deps.run(ctx, ffmpeg, analysisArgs)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg loudness analysis failed: %w: %s", err, compactCommandOutput(analysis))
	}
	stats, err := parseLoudnormStats(analysis)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg loudness analysis: %w", err)
	}

	output := filepath.Join(outDir, audioMixOutput)
	if err := removeStaleBuiltinOutput(output); err != nil {
		return nil, err
	}
	encodeArgs := audioMixEncodeArgs(inputs, output, stats)
	encoded, err := deps.run(ctx, ffmpeg, encodeArgs)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg mix encode failed: %w: %s", err, compactCommandOutput(encoded))
	}
	info, err := os.Stat(output)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, fmt.Errorf("ffmpeg reported success but did not write a non-empty %s", audioMixOutput)
	}
	return []string{audioMixOutput}, nil
}

type loudnormStats struct {
	InputI       string `json:"input_i"`
	InputTP      string `json:"input_tp"`
	InputLRA     string `json:"input_lra"`
	InputThresh  string `json:"input_thresh"`
	TargetOffset string `json:"target_offset"`
}

func parseLoudnormStats(output []byte) (loudnormStats, error) {
	var stats loudnormStats
	// ffmpeg writes logs around the JSON block on stderr. Walk candidate opening
	// braces from the end and accept only the loudnorm shape.
	for start := strings.LastIndexByte(string(output), '{'); start >= 0; start = strings.LastIndexByte(string(output[:start]), '{') {
		endRel := strings.IndexByte(string(output[start:]), '}')
		if endRel < 0 {
			continue
		}
		candidate := output[start : start+endRel+1]
		if json.Unmarshal(candidate, &stats) != nil || stats.InputI == "" || stats.InputTP == "" || stats.InputLRA == "" || stats.InputThresh == "" || stats.TargetOffset == "" {
			continue
		}
		for name, value := range map[string]string{
			"input_i": stats.InputI, "input_tp": stats.InputTP, "input_lra": stats.InputLRA,
			"input_thresh": stats.InputThresh, "target_offset": stats.TargetOffset,
		} {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return loudnormStats{}, fmt.Errorf("%s is not finite (%q); the stems may be silent", name, value)
			}
		}
		return stats, nil
	}
	return loudnormStats{}, fmt.Errorf("no loudnorm JSON statistics in ffmpeg output")
}

func audioMixInputArgs(inputs []string) []string {
	args := []string{"-hide_banner", "-nostdin", "-y"}
	for _, input := range inputs {
		args = append(args, "-i", input)
	}
	return args
}

func audioMixPrefix(inputCount int) string {
	var b strings.Builder
	for i := 0; i < inputCount; i++ {
		fmt.Fprintf(&b, "[%d:a]", i)
	}
	fmt.Fprintf(&b, "amix=inputs=%d:duration=longest:dropout_transition=0:normalize=0", inputCount)
	return b.String()
}

func audioMixAnalysisArgs(inputs []string) []string {
	filter := fmt.Sprintf("%s,loudnorm=I=%d:TP=%d:LRA=%d:print_format=json[mix]",
		audioMixPrefix(len(inputs)), audioMixTargetLUFS, audioMixTruePeakDB, audioMixLoudnessLRA)
	args := audioMixInputArgs(inputs)
	return append(args, "-filter_complex", filter, "-map", "[mix]", "-f", "null", "-")
}

func audioMixEncodeArgs(inputs []string, output string, s loudnormStats) []string {
	filter := fmt.Sprintf(
		"%s,loudnorm=I=%d:TP=%d:LRA=%d:measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true:print_format=summary[mix]",
		audioMixPrefix(len(inputs)), audioMixTargetLUFS, audioMixTruePeakDB, audioMixLoudnessLRA,
		s.InputI, s.InputTP, s.InputLRA, s.InputThresh, s.TargetOffset,
	)
	args := audioMixInputArgs(inputs)
	return append(args, "-filter_complex", filter, "-map", "[mix]", "-vn", "-c:a", "pcm_s24le", "-ar", "48000", output)
}

func compactCommandOutput(output []byte) string {
	s := strings.TrimSpace(string(output))
	const max = 2048
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}
