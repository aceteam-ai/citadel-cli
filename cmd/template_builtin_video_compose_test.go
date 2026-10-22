package cmd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const validVideoComposeFilm = `{"title":"Flight Log","subtitle":"A film","intro":{"lines":["AceTeam"],"seconds":4},"outro":{"lines":["Made with AceTeam"],"seconds":5},"theme":"blueprint","chapters":[{"file":"staged-a71f-chapter.mp4","title":"Chapter one","narrator":{"name":"the Ace","line":"Narrated by the Ace, the AI"},"narration_identity":"stock","speaks_as_person":false,"disclosure":null,"stamp":{"label":"SIGNED","by":"founder","at":"2026-10-12"},"card_seconds":2.5}]}`

// The platform merged these values in aceteam#11972 while the node source stayed
// inactive. Compare complete JSON values so unknown/missing wire fields fail too.
func TestVideoComposeSharedPlatformFixtures(t *testing.T) {
	compare := func(t *testing.T, name string, actual []byte) {
		t.Helper()
		want, err := os.ReadFile(filepath.Join("testdata", "film_compose", name))
		if err != nil {
			t.Fatal(err)
		}
		var wantValue, actualValue any
		if err := json.Unmarshal(want, &wantValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(actual, &actualValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actualValue, wantValue) {
			t.Fatalf("%s differs from merged aceteam#11972 fixture:\n%s", name, actual)
		}
	}
	t.Run("schema", func(t *testing.T) {
		compare(t, "renderer.film.schema.json", []byte(videoComposeFilmSchema))
	})
	t.Run("themes", func(t *testing.T) {
		reg, err := loadPapercraftSceneV2Registry()
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(reg.presets))
		for name := range reg.presets {
			names = append(names, name)
		}
		sort.Strings(names)
		raw, err := json.Marshal(names)
		if err != nil {
			t.Fatal(err)
		}
		compare(t, "renderer.themes.json", raw)
	})
	t.Run("timeline", func(t *testing.T) {
		inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
		outDir := t.TempDir()
		f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10)}, encodedBytes: videoComposeDefaultMaxBytes}
		if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, f.deps()); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(outDir, "chapters.json"))
		if err != nil {
			t.Fatal(err)
		}
		compare(t, "renderer.chapters.json", raw)
	})
}

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
		body := videoComposeTestMP4(true)
		if i == 0 {
			body = raw
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

// Structural fixture only: codec metadata in ordinary unit tests comes from the
// fake probe. The separate opt-in local proof uses a real encoded fixture.
func videoComposeTestBox(kind string, parts ...[]byte) []byte {
	payload := []byte{}
	for _, part := range parts {
		payload = append(payload, part...)
	}
	box := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(box[:4], uint32(8+len(payload)))
	copy(box[4:], kind)
	return append(box, payload...)
}
func videoComposeTestMP4(selfContained bool) []byte {
	flags := byte(1)
	if !selfContained {
		flags = 0
	}
	ref := videoComposeTestBox("url ", []byte{0, 0, 0, flags})
	dref := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, ref)
	track := videoComposeTestBox("trak", videoComposeTestBox("mdia", videoComposeTestBox("minf", videoComposeTestBox("dinf", dref))))
	return append(append(videoComposeTestBox("ftyp", []byte("isom\x00\x00\x02\x00isommp41")), videoComposeTestBox("moov", track, track)...), videoComposeTestBox("mdat", []byte{0})...)
}

func TestVideoComposeRejectsDisguisedDASHBeforeMedia(t *testing.T) {
	inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
	// An undeclared local MP4 sits outside the declared input directory. The
	// manifest points to it, but refusal must precede all media-tool invocation.
	outside := filepath.Join(t.TempDir(), "undeclared.mp4")
	if err := os.WriteFile(outside, videoComposeTestMP4(true), 0600); err != nil {
		t.Fatal(err)
	}
	dash := []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><Representation><BaseURL>` + outside + `</BaseURL></Representation></AdaptationSet></Period></MPD>`)
	if err := os.WriteFile(inputs[1], dash, 0600); err != nil {
		t.Fatal(err)
	}
	f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10)}}
	deps := f.deps()
	deps.look = func(string) (string, error) { t.Fatal("disguised DASH reached tool discovery"); return "", nil }
	deps.run = func(context.Context, string, []string) ([]byte, error) {
		t.Fatal("disguised DASH could consume undeclared media")
		return nil, nil
	}
	outDir := t.TempDir()
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, deps); err == nil {
		t.Fatal("disguised DASH accepted")
	}
	if f.rendered != 0 || len(f.calls) != 0 {
		t.Fatal("disguised DASH reached renderer/media tools")
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected manifest created outputs: %v %v", entries, err)
	}
	if err := os.WriteFile(inputs[1], videoComposeTestMP4(true), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := videoComposeInputs(inputs); err != nil {
		t.Fatalf("self-contained MP4 structure rejected: %v", err)
	}
}

func TestVideoComposeMP4RejectsExternalReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external.mp4")
	if err := os.WriteFile(path, videoComposeTestMP4(false), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateVideoComposeMP4(path); err == nil {
		t.Fatal("non-self-contained MP4 data reference accepted")
	}
	for _, body := range [][]byte{
		videoComposeTestBox("ftyp", []byte("isom\x00\x00\x00\x00")),
		append(videoComposeTestMP4(true), videoComposeTestBox("moof")...),
		append(videoComposeTestMP4(true), []byte{0, 0, 0, 1, 'f', 'r', 'e', 'e', 255, 255, 255, 255, 255, 255, 255, 255}...),
	} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateVideoComposeMP4(path); err == nil {
			t.Fatal("invalid/fragmented MP4 accepted")
		}
	}
}

func TestVideoComposeMP4InputArgsPinned(t *testing.T) {
	assert := func(args []string) {
		t.Helper()
		want := "-protocol_whitelist file -format_whitelist mov -f mov -enable_drefs 0 -use_absolute_path 0 -i input.mp4"
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Fatalf("MP4 input could autodetect a manifest/external reference: %v", args)
		}
	}
	assert(videoComposeMP4InputArgs("input.mp4"))
	assert(videoComposePrepareArgs("input.mp4", "out.mp4", videoComposeTestMedia(10), true))
	assert(videoComposePrepareArgs("input.mp4", "out.mp4", videoComposeTestMedia(10), false))
	assert(videoComposeLoudnormArgs("input.mp4", "", videoComposeLoudness{-14, -2, 11}, nil))
	args, err := videoComposeCapArgs("input.mp4", "out.mp4", 10, 48<<20)
	if err != nil {
		t.Fatal(err)
	}
	assert(args)
}

// Optional, bounded, offline proof. Supply a disposable genuinely encoded MP4;
// normal unit tests remain fake-command-only and never discover media tools.
func TestVideoComposeLocalEncodedMP4(t *testing.T) {
	fixture := os.Getenv("CITADEL_VIDEO_COMPOSE_OFFLINE_FIXTURE")
	if fixture == "" {
		t.Skip("set CITADEL_VIDEO_COMPOSE_OFFLINE_FIXTURE for bounded local proof")
	}
	info, err := os.Stat(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		t.Fatal("offline fixture must be a regular MP4 no larger than 1 MiB")
	}
	if err := validateVideoComposeMP4(fixture); err != nil {
		t.Fatalf("genuine encoded MP4 refused: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	probeArgs := append([]string{"-v", "error", "-show_streams", "-show_format", "-show_data_hash", "sha256", "-of", "json"}, videoComposeMP4InputArgs(fixture)...)
	raw, err := exec.CommandContext(ctx, ffprobe, probeArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("pinned genuine probe: %v %s", err, raw)
	}
	media, err := parseVideoComposeProbe(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, copyStreams := range []bool{true, false} {
		args := videoComposePrepareArgs(fixture, filepath.Join(t.TempDir(), "prepared.mp4"), media, copyStreams)
		if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("pinned genuine preparation: %v %s", err, out)
		}
	}
	// The undeclared BaseURL exists and contains valid streams. Nevertheless,
	// source preflight refuses it and forced MOV probing/preparation must fail.
	dash := filepath.Join(t.TempDir(), "disguised.mp4")
	manifest := []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><Representation><BaseURL>` + fixture + `</BaseURL></Representation></AdaptationSet></Period></MPD>`)
	if manifestPath := os.Getenv("CITADEL_VIDEO_COMPOSE_OFFLINE_MANIFEST"); manifestPath != "" {
		info, err := os.Stat(manifestPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			t.Fatal("offline manifest must be a regular file no larger than 64 KiB")
		}
		manifest, err = os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		// With the previous autodetection policy this genuine DASH fixture
		// supplies plausible H264/AAC metadata via undeclared local segments.
		baseline, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-protocol_whitelist", "file,pipe", "-show_streams", "-show_format", "-of", "json", "-i", manifestPath).CombinedOutput()
		if err != nil {
			t.Fatalf("offline baseline manifest: %v %s", err, baseline)
		}
		var value struct {
			Format struct {
				Name     string `json:"format_name"`
				Duration string `json:"duration"`
			} `json:"format"`
			Streams []struct {
				Codec string `json:"codec_name"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(baseline, &value); err != nil {
			t.Fatal(err)
		}
		if value.Format.Name != "dash" || value.Format.Duration == "0.000000" || len(value.Streams) != 2 || value.Streams[0].Codec != "h264" || value.Streams[1].Codec != "aac" {
			t.Fatalf("baseline does not reproduce manifest metadata confusion: %s", baseline)
		}
	}
	if err := os.WriteFile(dash, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateVideoComposeMP4(dash); err == nil {
		t.Fatal("DASH manifest passed MP4 preflight")
	}
	if out, err := exec.CommandContext(ctx, ffprobe, append([]string{"-v", "error"}, videoComposeMP4InputArgs(dash)...)...).CombinedOutput(); err == nil {
		t.Fatalf("forced MOV probe read undeclared BaseURL: %s", out)
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, videoComposePrepareArgs(dash, filepath.Join(t.TempDir(), "rejected.mp4"), media, true)...).CombinedOutput(); err == nil {
		t.Fatalf("forced MOV preparation read undeclared BaseURL: %s", out)
	}
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
	return []byte(fmt.Sprintf(`{"streams":[{"codec_type":"video","codec_name":%q,"width":%d,"height":%d,"r_frame_rate":%q,"pix_fmt":%q,"profile":%q,"level":%d,"time_base":%q,"sample_aspect_ratio":%q,"extradata_hash":%q},{"codec_type":"audio","codec_name":%q,"sample_rate":%q,"channels":%d,"channel_layout":%q,"extradata_hash":%q}],"format":{"duration":%q,"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`, m.VideoCodec, m.Width, m.Height, m.FPS, m.PixelFormat, m.Profile, m.Level, m.TimeBase, m.SampleAspect, m.ExtraDataHash, m.AudioCodec, m.SampleRate, m.Channels, m.ChannelLayout, m.AudioExtraDataHash, videoComposeNumber(m.Duration)))
}

type videoComposeFake struct {
	t                         *testing.T
	media                     map[string]videoComposeMedia
	calls                     [][]string
	encodedBytes, cappedBytes int64
	disclosures               int
	rendered                  int
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
	f.rendered++
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
		if !strings.Contains(strings.Join(args, " "), "-f mov -enable_drefs 0 -use_absolute_path 0 -i ") {
			f.t.Fatal("probe is not pinned to self-contained MOV")
		}
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
	if !concat && filepath.Ext(input) == ".mp4" && !strings.Contains(strings.Join(args, " "), "-f mov -enable_drefs 0 -use_absolute_path 0 -i ") {
		f.t.Fatal("ffmpeg MP4 input is not pinned")
	}
	if filepath.Ext(input) == ".png" && !strings.Contains(strings.Join(args, " "), "-f image2pipe -c:v png -i ") {
		f.t.Fatal("poster decoder is not pinned to PNG")
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
	if err := os.WriteFile(other, videoComposeTestMP4(true), 0600); err != nil {
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

func TestVideoComposeProbeRejectsWrongContainer(t *testing.T) {
	raw := strings.Replace(string(videoComposeTestProbe(videoComposeTestMedia(10))), `"format_name":"mov,mp4,m4a,3gp,3g2,mj2"`, `"format_name":"dash"`, 1)
	if _, err := parseVideoComposeProbe([]byte(raw)); err == nil {
		t.Fatal("valid stream metadata from a non-MP4 container accepted")
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
