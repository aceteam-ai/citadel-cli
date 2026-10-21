package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validVideoComposeFilm = `{"title":"Flight Log","subtitle":"A film","intro":{"lines":["AceTeam"],"seconds":4},"outro":{"lines":["Made with AceTeam"],"seconds":5},"theme":"blueprint","chapters":[{"file":"staged-a71f-chapter.mp4","title":"Chapter one","narrator":{"name":"the Ace","line":"Narrated by the Ace, the AI"},"narration_identity":"stock","speaks_as_person":false,"disclosure":null,"stamp":{"label":"SIGNED","by":"founder","at":"2026-10-12"},"card_seconds":2.5}]}`

func videoComposeTestFilm(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(validVideoComposeFilm), &value); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func videoComposeTestChapter(value map[string]any) map[string]any {
	return value["chapters"].([]any)[0].(map[string]any)
}

func TestVideoComposeFilmClosedSchema(t *testing.T) {
	film, err := decodeVideoComposeFilm([]byte(validVideoComposeFilm))
	if err != nil {
		t.Fatal(err)
	}
	if film.Delivery.MaxBytes != 50331648 || film.Loudness != (videoComposeLoudness{-14, -2, 11}) {
		t.Fatalf("wrong defaults: %+v", film)
	}
	boundary := videoComposeTestFilm(t, func(v map[string]any) {
		v["delivery"] = map[string]any{}
		v["loudness"] = map[string]any{"i": -16, "tp": -2, "lra": 11}
		chapters := []any{}
		for i := 0; i < 32; i++ {
			chapter := map[string]any{}
			for k, value := range videoComposeTestChapter(v) {
				chapter[k] = value
			}
			chapter["file"] = fmt.Sprintf("staged-%02d.mp4", i)
			chapters = append(chapters, chapter)
		}
		v["chapters"] = chapters
	})
	film, err = decodeVideoComposeFilm(boundary)
	if err != nil || film.Delivery.MaxBytes != 50331648 || film.Loudness.I != -16 || len(film.Chapters) != 32 {
		t.Fatalf("valid boundary rejected: %+v %v", film, err)
	}
	cases := map[string]func(map[string]any){
		"unknown_top":    func(v map[string]any) { v["bed"] = "music.wav" },
		"wrong_case":     func(v map[string]any) { v["Title"] = v["title"]; delete(v, "title") },
		"unknown_nested": func(v map[string]any) { videoComposeTestChapter(v)["narrator"].(map[string]any)["extra"] = true },
		"unknown_card":   func(v map[string]any) { v["intro"].(map[string]any)["extra"] = true },
		"zero_chapters":  func(v map[string]any) { v["chapters"] = []any{} },
		"too_many": func(v map[string]any) {
			ch := videoComposeTestChapter(v)
			many := []any{}
			for i := 0; i < 33; i++ {
				many = append(many, ch)
			}
			v["chapters"] = many
		},
		"missing_identity":   func(v map[string]any) { delete(videoComposeTestChapter(v), "narration_identity") },
		"missing_speaks":     func(v map[string]any) { delete(videoComposeTestChapter(v), "speaks_as_person") },
		"unknown_identity":   func(v map[string]any) { videoComposeTestChapter(v)["narration_identity"] = "cloned" },
		"null_speaks":        func(v map[string]any) { videoComposeTestChapter(v)["speaks_as_person"] = nil },
		"stock_as_person":    func(v map[string]any) { videoComposeTestChapter(v)["speaks_as_person"] = true },
		"blank_disclosure":   func(v map[string]any) { videoComposeTestChapter(v)["disclosure"] = "  " },
		"bad_path":           func(v map[string]any) { videoComposeTestChapter(v)["file"] = "../chapter.mp4" },
		"filter_path":        func(v map[string]any) { videoComposeTestChapter(v)["file"] = "chapter';evil.mp4" },
		"zero_duration":      func(v map[string]any) { videoComposeTestChapter(v)["card_seconds"] = 0 },
		"unknown_theme":      func(v map[string]any) { v["theme"] = "unknown" },
		"null_delivery":      func(v map[string]any) { v["delivery"] = nil },
		"zero_delivery":      func(v map[string]any) { v["delivery"] = map[string]any{"max_bytes": 0} },
		"excess_delivery":    func(v map[string]any) { v["delivery"] = map[string]any{"max_bytes": 50331649} },
		"null_loudness":      func(v map[string]any) { v["loudness"] = nil },
		"unsigned_stamp":     func(v map[string]any) { videoComposeTestChapter(v)["stamp"].(map[string]any)["label"] = "DRAFT" },
		"blank_signer":       func(v map[string]any) { videoComposeTestChapter(v)["stamp"].(map[string]any)["by"] = " " },
		"invalid_stamp_date": func(v map[string]any) { videoComposeTestChapter(v)["stamp"].(map[string]any)["at"] = "2026-02-30" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeVideoComposeFilm(videoComposeTestFilm(t, edit)); err == nil {
				t.Fatal("invalid film accepted")
			}
		})
	}
	for _, raw := range []string{strings.Replace(validVideoComposeFilm, `"title":"Flight Log"`, `"title":"first","title":"Flight Log"`, 1), validVideoComposeFilm + `{}`, `null`} {
		if _, err := decodeVideoComposeFilm([]byte(raw)); err == nil {
			t.Fatal("ambiguous/non-object JSON accepted")
		}
	}
	for _, identity := range []string{"stock", "consented_clone", "own_recording"} {
		raw := videoComposeTestFilm(t, func(v map[string]any) {
			ch := videoComposeTestChapter(v)
			ch["narration_identity"] = identity
			ch["speaks_as_person"] = true
			ch["disclosure"] = "Stock AI voice speaking as the founder."
		})
		if _, err := decodeVideoComposeFilm(raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVideoComposeDisclosureGuard(t *testing.T) {
	raw := videoComposeTestFilm(t, func(v map[string]any) { videoComposeTestChapter(v)["speaks_as_person"] = true })
	if _, err := decodeVideoComposeFilm(raw); err == nil || !strings.Contains(err.Error(), "requires disclosure") {
		t.Fatalf("guard did not refuse stock-as-person: %v", err)
	}
}

func videoComposeTestInputs(t *testing.T, raw []byte) []string {
	t.Helper()
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "film.json"), filepath.Join(dir, "staged-a71f-chapter.mp4")}
	for i, path := range paths {
		body := []byte("fake MP4 fixture")
		if i == 0 {
			body = raw
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestVideoComposeBindsDeclaredFinalBasenames(t *testing.T) {
	inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
	_, chapters, _, err := videoComposeInputs(inputs)
	if err != nil || !reflect.DeepEqual(chapters, inputs[1:]) {
		t.Fatalf("binding: %v %v", chapters, err)
	}
	if _, _, _, err := videoComposeInputs(inputs[:1]); err == nil {
		t.Fatal("missing chapter accepted")
	}
	for _, name := range []string{"ch1.mp4", "STAGED-a71f-chapter.mp4"} {
		raw := videoComposeTestFilm(t, func(v map[string]any) { videoComposeTestChapter(v)["file"] = name })
		if err := os.WriteFile(inputs[0], raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := videoComposeInputs(inputs); err == nil {
			t.Fatalf("unbound basename %q accepted", name)
		}
	}
}

func TestVideoComposeInactiveBeforeEffects(t *testing.T) {
	inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
	outDir := t.TempDir()
	deps := liveVideoComposeDeps()
	deps.look = func(string) (string, error) { t.Fatal("inactive runner discovered a media tool"); return "", nil }
	deps.run = func(context.Context, string, []string) ([]byte, error) {
		t.Fatal("inactive runner invoked media")
		return nil, nil
	}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, deps); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("missing dependency gate: %v", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("inactive runner left outputs: %v %v", entries, err)
	}
	if _, ok := lookupBuiltinTemplateRunner("video-compose"); ok {
		t.Fatal("incomplete video-compose was registered")
	}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{"bed":true}`), inputs, outDir, deps); err == nil {
		t.Fatal("params override accepted")
	}
}

func videoComposeTestMedia(duration float64) videoComposeMedia {
	return videoComposeMedia{Width: 1920, Height: 1080, FPS: "30/1", VideoCodec: "h264", PixelFormat: "yuv420p", Profile: "High", Level: 40, TimeBase: "1/15360", SampleAspect: "1:1", AudioCodec: "aac", SampleRate: "48000", ChannelLayout: "stereo", Channels: 2, ExtraDataHash: "SHA256:fixture", AudioExtraDataHash: "SHA256:audio", Duration: duration}
}
func videoComposeTestProbe(m videoComposeMedia) []byte {
	return []byte(fmt.Sprintf(`{"streams":[{"codec_type":"video","codec_name":%q,"width":%d,"height":%d,"r_frame_rate":%q,"pix_fmt":%q,"profile":%q,"level":%d,"time_base":%q,"sample_aspect_ratio":%q,"extradata_hash":%q},{"codec_type":"audio","codec_name":%q,"sample_rate":%q,"channels":%d,"channel_layout":%q,"extradata_hash":%q}],"format":{"duration":%q}}`, m.VideoCodec, m.Width, m.Height, m.FPS, m.PixelFormat, m.Profile, m.Level, m.TimeBase, m.SampleAspect, m.ExtraDataHash, m.AudioCodec, m.SampleRate, m.Channels, m.ChannelLayout, m.AudioExtraDataHash, videoComposeNumber(m.Duration)))
}

type videoComposeFake struct {
	t                         *testing.T
	media                     map[string]videoComposeMedia
	calls                     [][]string
	encodedBytes, cappedBytes int64
	disclosures               int
}

func (f *videoComposeFake) write(path string, m videoComposeMedia, size int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		return err
	}
	f.media[path] = m
	return file.Close()
}
func (f *videoComposeFake) RenderCard(_ context.Context, film videoComposeFilm, index int, ref videoComposeMedia, path string) error {
	duration := film.Intro.Seconds
	if index == len(film.Chapters) {
		duration = film.Outro.Seconds
	} else if index >= 0 {
		duration = film.Chapters[index].CardSeconds
	}
	ref.Duration = duration
	ref.VideoCodec = "h264"
	ref.AudioCodec = "aac"
	return f.write(path, ref, 3)
}
func (f *videoComposeFake) BurnDisclosure(_ context.Context, ch videoComposeChapter, ref videoComposeMedia, input, output string) error {
	if ch.Disclosure == nil {
		return fmt.Errorf("no disclosure")
	}
	f.disclosures++
	ref.Duration = f.media[input].Duration
	return f.write(output, ref, 3)
}
func (f *videoComposeFake) run(_ context.Context, bin string, args []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{bin}, args...))
	if bin == "ffprobe" {
		m, ok := f.media[args[len(args)-1]]
		if !ok {
			return nil, fmt.Errorf("no fake probe for %s", args[len(args)-1])
		}
		return videoComposeTestProbe(m), nil
	}
	output := args[len(args)-1]
	if output == "-" {
		return []byte(`{"input_i":"-20","input_tp":"-5","input_lra":"3","input_thresh":"-30","target_offset":"0.2"}`), nil
	}
	input := ""
	concat := false
	for i, a := range args {
		if a == "-i" {
			input = args[i+1]
		}
		if a == "concat" {
			concat = true
		}
	}
	m := f.media[input]
	if concat {
		raw, err := os.ReadFile(input)
		if err != nil {
			return nil, err
		}
		m = videoComposeTestMedia(0)
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "file '"), "'")
			m.Duration += f.media[filepath.Join(filepath.Dir(input), name)].Duration
		}
	}
	if strings.Contains(strings.Join(args, " "), "libx264") {
		ref := videoComposeTestMedia(m.Duration)
		m = ref
	}
	size := int64(3)
	if filepath.Base(output) == "film.mp4" {
		size = f.encodedBytes
	}
	if filepath.Base(output) == "capped.mp4" {
		size = f.cappedBytes
	}
	return nil, f.write(output, m, size)
}
func (f *videoComposeFake) deps() videoComposeDeps {
	return videoComposeDeps{look: func(name string) (string, error) { return name, nil }, run: f.run, visuals: f}
}
func (f *videoComposeFake) ffmpegCalls() [][]string {
	out := [][]string{}
	for _, call := range f.calls {
		if call[0] == "ffmpeg" {
			out = append(out, call)
		}
	}
	return out
}

func TestVideoComposeFakeMediaPipeline(t *testing.T) {
	for _, capped := range []bool{false, true} {
		t.Run(fmt.Sprint(capped), func(t *testing.T) {
			raw := videoComposeTestFilm(t, func(v map[string]any) { v["delivery"] = map[string]any{"max_bytes": 1 << 20} })
			inputs := videoComposeTestInputs(t, raw)
			outDir := t.TempDir()
			f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10)}, encodedBytes: 1 << 20, cappedBytes: (1 << 20) - 1}
			if capped {
				f.encodedBytes++
			}
			outputs, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, f.deps())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(outputs, []string{"film.mp4", "poster.png", "chapters.json"}) {
				t.Fatalf("outputs: %v", outputs)
			}
			var timeline videoComposeTimeline
			body, err := os.ReadFile(filepath.Join(outDir, "chapters.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &timeline); err != nil {
				t.Fatal(err)
			}
			if timeline.Duration != 21.5 || len(timeline.Chapters) != 1 || timeline.Chapters[0].CardStart != 4 || timeline.Chapters[0].Start != 6.5 || timeline.Chapters[0].End != 16.5 || timeline.Chapters[0].End+5 != timeline.Duration {
				t.Fatalf("wrong timeline: %+v", timeline)
			}
			passes := 1
			if capped {
				passes = 2
			}
			if timeline.EncodePasses != passes || timeline.Bytes > 1<<20 {
				t.Fatalf("wrong cap accounting: %+v", timeline)
			}
			if timeline.LoudnessMeasuredStage != "pre_normalization" {
				t.Fatal("input loudness analysis was not labeled as pre-normalization")
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			var measured map[string]string
			if err := json.Unmarshal(envelope["loudness_measured"], &measured); err != nil {
				t.Fatal(err)
			}
			if len(measured) != 5 || measured["input_i"] != "-20" || measured["input_tp"] != "-5" || measured["input_lra"] != "3" || measured["input_thresh"] != "-30" || measured["target_offset"] != "0.2" {
				t.Fatalf("unexpected loudness provenance: %v", measured)
			}
			joined := ""
			for _, call := range f.ffmpegCalls() {
				joined += strings.Join(call, " ") + "\n"
			}
			for _, needle := range []string{"-c copy", "-f concat -safe 1", "loudnorm=I=-14.000000000:TP=-2.000000000:LRA=11.000000000:print_format=json", "measured_I=-20:measured_TP=-5:measured_LRA=3:measured_thresh=-30:offset=0.2:linear=true", "-c:v copy", "-movflags +faststart", "-ss 4.000000000"} {
				if !strings.Contains(joined, needle) {
					t.Fatalf("missing argv %q:\n%s", needle, joined)
				}
			}
			if strings.Contains(joined, "-maxrate") != capped {
				t.Fatal("cap pass must run only above max_bytes")
			}
			entries, err := os.ReadDir(outDir)
			if err != nil || len(entries) != 3 {
				t.Fatalf("scratch retained: %v %v", entries, err)
			}
		})
	}
}

func TestVideoComposeMixedAspectRefused(t *testing.T) {
	raw := videoComposeTestFilm(t, func(v map[string]any) {
		ch := videoComposeTestChapter(v)
		copy := map[string]any{}
		for k, val := range ch {
			copy[k] = val
		}
		copy["file"] = "other.mp4"
		v["chapters"] = append(v["chapters"].([]any), copy)
	})
	inputs := videoComposeTestInputs(t, raw)
	other := filepath.Join(filepath.Dir(inputs[0]), "other.mp4")
	if err := os.WriteFile(other, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, other)
	b := videoComposeTestMedia(10)
	b.Width, b.Height = 1920, 1200
	f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10), other: b}}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, t.TempDir(), f.deps()); err == nil || !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("mixed aspect accepted: %v", err)
	}
	if len(f.ffmpegCalls()) != 0 {
		t.Fatal("mixed aspect rendered media")
	}
}

func TestVideoComposeStreamDecision(t *testing.T) {
	a := videoComposeTestMedia(10)
	b := a
	b.Duration = 15
	if !videoComposeCopyCompatible(a, b) {
		t.Fatal("duration prevented stream-copy")
	}
	for _, change := range []func(*videoComposeMedia){func(m *videoComposeMedia) { m.FPS = "24/1" }, func(m *videoComposeMedia) { m.SampleRate = "44100" }, func(m *videoComposeMedia) { m.PixelFormat = "yuv444p" }, func(m *videoComposeMedia) { m.ExtraDataHash = "other" }, func(m *videoComposeMedia) { m.TimeBase = "1/1000" }, func(m *videoComposeMedia) { m.Width = 1280; m.Height = 720 }} {
		b = a
		change(&b)
		if videoComposeCopyCompatible(a, b) {
			t.Fatalf("mismatched streams copied: %+v", b)
		}
		args := strings.Join(videoComposePrepareArgs("input.mp4", "output.mp4", a, false), " ")
		if !strings.Contains(args, "libx264 -crf 17") || !strings.Contains(args, "-ar 48000") {
			t.Fatalf("bad normalize argv: %s", args)
		}
	}
}

func TestVideoComposeProbeRejectsInvalidMetadata(t *testing.T) {
	for name, edit := range map[string]func(*videoComposeMedia){
		"duration":      func(m *videoComposeMedia) { m.Duration = 0 },
		"fps":           func(m *videoComposeMedia) { m.FPS = "30/0" },
		"portrait":      func(m *videoComposeMedia) { m.Width, m.Height = 1080, 1920 },
		"sample_rate":   func(m *videoComposeMedia) { m.SampleRate = "invalid" },
		"channels":      func(m *videoComposeMedia) { m.Channels = 0 },
		"sample_aspect": func(m *videoComposeMedia) { m.SampleAspect = "2:1" },
	} {
		t.Run(name, func(t *testing.T) {
			m := videoComposeTestMedia(10)
			edit(&m)
			if _, err := parseVideoComposeProbe(videoComposeTestProbe(m)); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestVideoComposeReencodeAndPosterOverride(t *testing.T) {
	inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
	poster := filepath.Join(filepath.Dir(inputs[0]), "poster.png")
	if err := os.WriteFile(poster, []byte("fake poster fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, poster)
	media := videoComposeTestMedia(10)
	media.VideoCodec = "mpeg4"
	f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: media}, encodedBytes: 3}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, t.TempDir(), f.deps()); err != nil {
		t.Fatal(err)
	}
	reencoded, override := false, false
	for _, call := range f.ffmpegCalls() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "-c:v libx264 -crf 17") {
			reencoded = true
		}
		if strings.Contains(joined, "-i "+poster+" -frames:v 1") {
			override = true
		}
	}
	if !reencoded || !override {
		t.Fatalf("missing normalization/poster override: %v", f.calls)
	}
}

func TestVideoComposeDisclosureAndCapFailure(t *testing.T) {
	raw := videoComposeTestFilm(t, func(v map[string]any) {
		ch := videoComposeTestChapter(v)
		ch["speaks_as_person"] = true
		ch["disclosure"] = "AI stock voice"
		v["delivery"] = map[string]any{"max_bytes": 1 << 20}
	})
	inputs := videoComposeTestInputs(t, raw)
	f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10)}, encodedBytes: (1 << 20) + 1, cappedBytes: (1 << 20) + 1}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, t.TempDir(), f.deps()); err == nil || !strings.Contains(err.Error(), "still exceeds") {
		t.Fatalf("cap overrun accepted: %v", err)
	}
	if f.disclosures != 1 {
		t.Fatal("chapter disclosure was not passed to renderer")
	}
	if _, err := videoComposeCapArgs("in", "out", 21.5, 1); err == nil {
		t.Fatal("impossible delivery budget accepted")
	}
}

func TestVideoComposeSingleLandscapeCapture(t *testing.T) {
	inDir, outDir := t.TempDir(), t.TempDir()
	html := writePapercraftHTML(t, inDir, "card.html", 1920, 1080)
	pages, encoders := 0, 0
	deps := papercraftRenderDeps{startPage: func(_ context.Context, _ string, w, h int, _ string, _ []string) (papercraftPage, error) {
		pages++
		if w != 1920 || h != 1080 {
			t.Fatalf("unexpected format %dx%d", w, h)
		}
		return &fakePapercraftPage{duration: 2.5}, nil
	}, startEncoder: func(_ context.Context, cfg papercraftEncoderConfig) (papercraftEncoder, error) {
		encoders++
		if !cfg.SilentAudio || !strings.Contains(strings.Join(papercraftFFmpegArgs(cfg), " "), "anullsrc=r=48000:cl=stereo") {
			t.Fatal("card capture is missing continuous silent audio")
		}
		return &fakePapercraftEncoder{cfg: cfg}, nil
	}}
	outputs, err := renderPapercraftLandscapeCard(context.Background(), html, outDir, 1920, 1080, 2.5, deps)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 1 || encoders != 1 || !reflect.DeepEqual(outputs, []string{"card.mp4"}) {
		t.Fatalf("single card emitted extra formats: %v %d %d", outputs, pages, encoders)
	}
	if _, err := os.Stat(filepath.Join(outDir, "portrait.mp4")); !os.IsNotExist(err) {
		t.Fatal("portrait emitted")
	}
}
