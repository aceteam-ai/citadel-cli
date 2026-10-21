package cmd

// Source-only preparation for aceteam-ai/aceteam#11481. There is deliberately
// no init/registry entry: the v3 card shell and full-duration disclosure renderer
// have not landed. Supplying a test renderer does not make this template ready.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

const videoComposeDefaultMaxBytes int64 = 48 << 20
const videoComposeMaxFilmJSON = 1 << 20

// Required identity fields repair the omission in #11481's example. Their enum
// is shared with #11484. Validate exact keys before Go's case-insensitive binder.
const videoComposeFilmSchema = `{
 "type":"object","additionalProperties":false,
 "required":["title","subtitle","intro","outro","theme","chapters"],
 "properties":{
  "title":{"$ref":"#/$defs/text"},"subtitle":{"type":"string","maxLength":500},
  "theme":{"type":"string","minLength":1,"maxLength":80},
  "intro":{"$ref":"#/$defs/card"},"outro":{"$ref":"#/$defs/card"},
  "chapters":{"type":"array","minItems":1,"maxItems":32,"items":{
   "type":"object","additionalProperties":false,
   "required":["file","title","narrator","narration_identity","speaks_as_person","disclosure","stamp","card_seconds"],
   "properties":{
    "file":{"type":"string","minLength":1,"maxLength":255},"title":{"$ref":"#/$defs/text"},
    "narration_identity":{"type":"string","enum":["stock","consented_clone","own_recording"]},"speaks_as_person":{"type":"boolean"},
    "disclosure":{"type":["string","null"],"minLength":1,"maxLength":1000},
    "card_seconds":{"type":"number","exclusiveMinimum":0,"maximum":60},
    "narrator":{"type":"object","additionalProperties":false,"required":["name","line"],"properties":{"name":{"$ref":"#/$defs/text"},"line":{"$ref":"#/$defs/text"}}},
    "stamp":{"type":"object","additionalProperties":false,"required":["label","by","at"],"properties":{"label":{"type":"string","enum":["SIGNED"]},"by":{"$ref":"#/$defs/text"},"at":{"$ref":"#/$defs/text"}}}
   }}},
  "loudness":{"type":"object","additionalProperties":false,"required":["i","tp","lra"],"properties":{"i":{"type":"number","minimum":-70,"maximum":-5},"tp":{"type":"number","minimum":-9,"maximum":0},"lra":{"type":"number","minimum":1,"maximum":50}}},
  "delivery":{"type":"object","additionalProperties":false,"properties":{"max_bytes":{"type":"integer","minimum":1,"maximum":50331648}}}
 },
 "$defs":{
  "text":{"type":"string","minLength":1,"maxLength":1000},
  "card":{"type":"object","additionalProperties":false,"required":["lines","seconds"],"properties":{"lines":{"type":"array","minItems":1,"maxItems":12,"items":{"$ref":"#/$defs/text"}},"seconds":{"type":"number","exclusiveMinimum":0,"maximum":60}}}
 }
}`

type videoComposeCard struct {
	Lines   []string `json:"lines"`
	Seconds float64  `json:"seconds"`
}
type videoComposeNarrator struct {
	Name string `json:"name"`
	Line string `json:"line"`
}
type videoComposeStamp struct {
	Label string `json:"label"`
	By    string `json:"by"`
	At    string `json:"at"`
}
type videoComposeChapter struct {
	File              string               `json:"file"`
	Title             string               `json:"title"`
	Narrator          videoComposeNarrator `json:"narrator"`
	NarrationIdentity string               `json:"narration_identity"`
	SpeaksAsPerson    bool                 `json:"speaks_as_person"`
	Disclosure        *string              `json:"disclosure"`
	Stamp             videoComposeStamp    `json:"stamp"`
	CardSeconds       float64              `json:"card_seconds"`
}
type videoComposeLoudness struct {
	I   float64 `json:"i"`
	TP  float64 `json:"tp"`
	LRA float64 `json:"lra"`
}
type videoComposeFilm struct {
	Title    string                `json:"title"`
	Subtitle string                `json:"subtitle"`
	Intro    videoComposeCard      `json:"intro"`
	Outro    videoComposeCard      `json:"outro"`
	Theme    string                `json:"theme"`
	Chapters []videoComposeChapter `json:"chapters"`
	Loudness videoComposeLoudness  `json:"loudness"`
	Delivery struct {
		MaxBytes int64 `json:"max_bytes"`
	} `json:"delivery"`
}

func decodeVideoComposeFilm(raw []byte) (videoComposeFilm, error) {
	var film videoComposeFilm
	if len(raw) > videoComposeMaxFilmJSON {
		return film, fmt.Errorf("film.json exceeds 1 MiB")
	}
	v, err := jobs.DecodeTemplateJSON(raw)
	if err != nil {
		return film, err
	}
	schema, err := jsonschema.CompileString("film.json", videoComposeFilmSchema)
	if err != nil {
		return film, err
	}
	if err := schema.Validate(v); err != nil {
		return film, fmt.Errorf("film.json: %w", err)
	}
	film.Loudness = videoComposeLoudness{-14, -2, 11}
	film.Delivery.MaxBytes = videoComposeDefaultMaxBytes
	if err := json.Unmarshal(raw, &film); err != nil {
		return film, err
	}
	reg, err := loadPapercraftSceneV2Registry()
	if err != nil {
		return film, err
	}
	if !reg.presets[film.Theme] {
		return film, fmt.Errorf("unknown film theme %q", film.Theme)
	}
	seen := map[string]bool{}
	for _, ch := range film.Chapters {
		if !videoComposeSafeBasename(ch.File) || filepath.Ext(ch.File) != ".mp4" || seen[ch.File] {
			return film, fmt.Errorf("chapter file must be a unique staged .mp4 basename: %q", ch.File)
		}
		seen[ch.File] = true
		if strings.TrimSpace(ch.Stamp.By) == "" {
			return film, fmt.Errorf("stamp.by must not be blank")
		}
		if _, err := time.Parse("2006-01-02", ch.Stamp.At); err != nil {
			return film, fmt.Errorf("stamp.at must be a valid YYYY-MM-DD date")
		}
		if ch.Disclosure != nil && strings.TrimSpace(*ch.Disclosure) == "" {
			return film, fmt.Errorf("disclosure must not be blank")
		}
		if ch.NarrationIdentity == "stock" && ch.SpeaksAsPerson && ch.Disclosure == nil {
			return film, fmt.Errorf("stock narration speaking as a person requires disclosure")
		}
	}
	return film, nil
}

func videoComposeSafeBasename(name string) bool {
	if name == "" || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// Bind only declared staged inputs, using their exact final basenames. Never
// resolve an arbitrary file named in film.json against the node filesystem.
func videoComposeInputs(inputs []string) (videoComposeFilm, []string, string, error) {
	var film videoComposeFilm
	files := map[string]string{}
	for _, path := range inputs {
		base := filepath.Base(path)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || files[base] != "" {
			return film, nil, "", fmt.Errorf("input %q must be a unique regular staged file", base)
		}
		files[base] = path
	}
	path := files["film.json"]
	if path == "" {
		return film, nil, "", fmt.Errorf("exactly one film.json is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return film, nil, "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, videoComposeMaxFilmJSON+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return film, nil, "", err
	}
	film, err = decodeVideoComposeFilm(raw)
	if err != nil {
		return film, nil, "", err
	}
	delete(files, "film.json")
	poster := files["poster.png"]
	delete(files, "poster.png")
	chapters := make([]string, 0, len(film.Chapters))
	for _, ch := range film.Chapters {
		if files[ch.File] == "" {
			return film, nil, "", fmt.Errorf("chapter %q is not a declared input", ch.File)
		}
		chapters = append(chapters, files[ch.File])
		delete(files, ch.File)
	}
	if len(files) != 0 {
		return film, nil, "", fmt.Errorf("undeclared extra inputs are not supported")
	}
	return film, chapters, poster, nil
}

type videoComposeMedia struct {
	Width, Height                                                                                                                           int
	FPS, VideoCodec, PixelFormat, Profile, TimeBase, SampleAspect, AudioCodec, SampleRate, ChannelLayout, ExtraDataHash, AudioExtraDataHash string
	Level, Channels                                                                                                                         int
	Duration                                                                                                                                float64
}

func parseVideoComposeProbe(raw []byte) (videoComposeMedia, error) {
	var probe struct {
		Streams []struct {
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			Width         int    `json:"width"`
			Height        int    `json:"height"`
			FPS           string `json:"r_frame_rate"`
			PixelFormat   string `json:"pix_fmt"`
			Profile       string `json:"profile"`
			Level         int    `json:"level"`
			TimeBase      string `json:"time_base"`
			SampleAspect  string `json:"sample_aspect_ratio"`
			SampleRate    string `json:"sample_rate"`
			Channels      int    `json:"channels"`
			ChannelLayout string `json:"channel_layout"`
			ExtraDataHash string `json:"extradata_hash"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	var m videoComposeMedia
	if err := json.Unmarshal(raw, &probe); err != nil {
		return m, err
	}
	if len(probe.Streams) != 2 {
		return m, fmt.Errorf("chapter requires exactly one video and one audio stream")
	}
	video, audio := 0, 0
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			video++
			m.Width, m.Height, m.FPS, m.VideoCodec, m.PixelFormat = s.Width, s.Height, s.FPS, s.CodecName, s.PixelFormat
			m.Profile, m.Level, m.TimeBase, m.SampleAspect, m.ExtraDataHash = s.Profile, s.Level, s.TimeBase, s.SampleAspect, s.ExtraDataHash
		case "audio":
			audio++
			m.AudioCodec, m.SampleRate, m.Channels, m.ChannelLayout = s.CodecName, s.SampleRate, s.Channels, s.ChannelLayout
			m.AudioExtraDataHash = s.ExtraDataHash
		}
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	fps, fpsErr := videoComposeFPS(m.FPS)
	sampleRate, sampleErr := strconv.Atoi(m.SampleRate)
	if err != nil || !videoComposeFinite(duration) || duration <= 0 || duration > 33*papercraftMaxDuration || video != 1 || audio != 1 || m.Width <= m.Height || m.Width > 8192 || m.Height < 2 || m.Width%2 != 0 || m.Height%2 != 0 || fpsErr != nil || fps < 1 || fps > 120 || m.VideoCodec == "" || m.AudioCodec == "" || sampleErr != nil || sampleRate < 8000 || sampleRate > 192000 || m.Channels < 1 || m.Channels > 8 || m.SampleAspect != "1:1" {
		return m, fmt.Errorf("unsupported or invalid landscape chapter stream metadata")
	}
	m.Duration = duration
	return m, nil
}

func videoComposeFPS(value string) (float64, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid rational fps")
	}
	n, e1 := strconv.ParseFloat(parts[0], 64)
	d, e2 := strconv.ParseFloat(parts[1], 64)
	if e1 != nil || e2 != nil || !videoComposeFinite(n) || !videoComposeFinite(d) || d <= 0 {
		return 0, fmt.Errorf("invalid rational fps")
	}
	return n / d, nil
}
func videoComposeFinite(v float64) bool   { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func videoComposeNumber(v float64) string { return strconv.FormatFloat(v, 'f', 9, 64) }
func videoComposeSameAspect(a, b videoComposeMedia) bool {
	return int64(a.Width)*int64(b.Height) == int64(b.Width)*int64(a.Height)
}
func videoComposeCopyCompatible(a, b videoComposeMedia) bool {
	// Duration is the only field irrelevant to the stream-copy decision. Missing
	// H264 extradata fails closed; matching dimensions/fps alone is insufficient.
	a.Duration, b.Duration = 0, 0
	return a == b && a.VideoCodec == "h264" && a.AudioCodec == "aac" && a.ExtraDataHash != "" && a.AudioExtraDataHash != ""
}

type videoComposeRow struct {
	Index      int                  `json:"index"`
	Title      string               `json:"title"`
	Start      float64              `json:"start_s"`
	End        float64              `json:"end_s"`
	CardStart  float64              `json:"card_start_s"`
	Narrator   videoComposeNarrator `json:"narrator"`
	Stamp      videoComposeStamp    `json:"stamp"`
	Disclosure *string              `json:"disclosure"`
}
type videoComposeTimeline struct {
	Chapters              []videoComposeRow `json:"chapters"`
	Duration              float64           `json:"duration_s"`
	Bytes                 int64             `json:"bytes"`
	LoudnessMeasured      loudnormStats     `json:"loudness_measured"`
	LoudnessMeasuredStage string            `json:"loudness_measured_stage"`
	EncodePasses          int               `json:"encode_passes"`
}

// Future production implementation must use the v3 single-landscape card path,
// carry each disclosure on its chapter card, burn it on every chapter frame,
// and return a continuous silent audio track for each card. No implementation
// is supplied here: neither alternate HTML nor invented card frames are valid.
type videoComposeVisuals interface {
	RenderCard(context.Context, videoComposeFilm, int, videoComposeMedia, string) error
	BurnDisclosure(context.Context, videoComposeChapter, videoComposeMedia, string, string) error
}
type videoComposeDeps struct {
	look    func(string) (string, error)
	run     func(context.Context, string, []string) ([]byte, error)
	visuals videoComposeVisuals
}

func liveVideoComposeDeps() videoComposeDeps {
	return videoComposeDeps{look: exec.LookPath, run: liveAudioMixDeps().run}
}

func runVideoComposeBuiltin(ctx context.Context, params json.RawMessage, inputs []string, outDir string, deps videoComposeDeps) (outputs []string, retErr error) {
	ctx, cancel := context.WithTimeout(ctx, renderJobWallTimeout)
	defer cancel()
	if _, err := decodeBuiltinParams(params); err != nil {
		return nil, err
	}
	film, chapters, poster, err := videoComposeInputs(inputs)
	if err != nil {
		return nil, err
	}
	// Dependency refusal precedes tool discovery, subprocesses and output writes.
	if deps.visuals == nil {
		return nil, fmt.Errorf("video-compose inactive: requires #11480 card scenes and #11296 v3 shell/disclosure integration")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ffprobe, err := deps.look("ffprobe")
	if err != nil {
		return nil, err
	}
	ffmpeg, err := deps.look("ffmpeg")
	if err != nil {
		return nil, err
	}
	probe := func(path string) (videoComposeMedia, error) {
		raw, err := deps.run(ctx, ffprobe, []string{"-v", "error", "-protocol_whitelist", "file,pipe", "-show_streams", "-show_format", "-show_data_hash", "sha256", "-of", "json", "-i", path})
		if err != nil {
			return videoComposeMedia{}, fmt.Errorf("ffprobe failed: %w", err)
		}
		return parseVideoComposeProbe(raw)
	}
	media := make([]videoComposeMedia, len(chapters))
	for i, path := range chapters {
		media[i], err = probe(path)
		if err != nil {
			return nil, err
		}
		if i > 0 && !videoComposeSameAspect(media[0], media[i]) {
			return nil, fmt.Errorf("mixed chapter aspect ratios")
		}
	}
	scratch, err := os.MkdirTemp(outDir, ".video-compose-")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	// Render the reference intro first; its canonical encoder output is the
	// reference for cards and chapters, rather than unsafe codec-name guessing.
	intro := filepath.Join(scratch, "intro.mp4")
	if err := deps.visuals.RenderCard(ctx, film, -1, media[0], intro); err != nil {
		return nil, err
	}
	reference, err := probe(intro)
	if err != nil {
		return nil, err
	}
	if reference.Width != media[0].Width || reference.Height != media[0].Height || reference.FPS != media[0].FPS {
		return nil, fmt.Errorf("card renderer changed reference dimensions or fps")
	}
	run := func(args []string) error {
		out, err := deps.run(ctx, ffmpeg, args)
		if err != nil {
			return fmt.Errorf("video-compose ffmpeg: %w: %s", err, compactCommandOutput(out))
		}
		return nil
	}
	timeline := videoComposeTimeline{Duration: reference.Duration}
	playlist := "file 'intro.mp4'\n"
	for i, ch := range film.Chapters {
		cardName := fmt.Sprintf("card-%02d.mp4", i)
		cardPath := filepath.Join(scratch, cardName)
		if err := deps.visuals.RenderCard(ctx, film, i, reference, cardPath); err != nil {
			return nil, err
		}
		card, err := probe(cardPath)
		if err != nil {
			return nil, err
		}
		if !videoComposeCopyCompatible(reference, card) {
			return nil, fmt.Errorf("card stream parameters do not match intro")
		}
		row := videoComposeRow{Index: i + 1, Title: ch.Title, CardStart: timeline.Duration, Narrator: ch.Narrator, Stamp: ch.Stamp, Disclosure: ch.Disclosure}
		timeline.Duration += card.Duration
		row.Start = timeline.Duration
		clipName := fmt.Sprintf("clip-%02d.mp4", i)
		clipPath := filepath.Join(scratch, clipName)
		if ch.Disclosure != nil {
			if err := deps.visuals.BurnDisclosure(ctx, ch, reference, chapters[i], clipPath); err != nil {
				return nil, err
			}
		} else if err := run(videoComposePrepareArgs(chapters[i], clipPath, reference, videoComposeCopyCompatible(reference, media[i]))); err != nil {
			return nil, err
		}
		actual, err := probe(clipPath)
		if err != nil {
			return nil, err
		}
		if !videoComposeCopyCompatible(reference, actual) {
			return nil, fmt.Errorf("prepared chapter stream parameters do not match cards")
		}
		timeline.Duration += actual.Duration
		row.End = timeline.Duration
		timeline.Chapters = append(timeline.Chapters, row)
		playlist += "file '" + cardName + "'\nfile '" + clipName + "'\n"
	}
	outro := filepath.Join(scratch, "outro.mp4")
	if err := deps.visuals.RenderCard(ctx, film, len(chapters), reference, outro); err != nil {
		return nil, err
	}
	end, err := probe(outro)
	if err != nil {
		return nil, err
	}
	if !videoComposeCopyCompatible(reference, end) {
		return nil, fmt.Errorf("outro stream parameters do not match intro")
	}
	timeline.Duration += end.Duration
	playlist += "file 'outro.mp4'\n"
	list := filepath.Join(scratch, "concat.txt")
	if err := os.WriteFile(list, []byte(playlist), 0o600); err != nil {
		return nil, err
	}
	joined := filepath.Join(scratch, "joined.mp4")
	if err := run([]string{"-hide_banner", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-f", "concat", "-safe", "1", "-i", list, "-map", "0:v:0", "-map", "0:a:0", "-c", "copy", joined}); err != nil {
		return nil, err
	}
	analysis, err := deps.run(ctx, ffmpeg, videoComposeLoudnormArgs(joined, "", film.Loudness, nil))
	if err != nil {
		return nil, err
	}
	timeline.LoudnessMeasured, err = parseLoudnormStats(analysis)
	if err != nil {
		return nil, err
	}
	timeline.LoudnessMeasuredStage = "pre_normalization"
	filmPath := filepath.Join(outDir, "film.mp4")
	if err := removeStaleBuiltinOutput(filmPath); err != nil {
		return nil, err
	}
	if err := run(videoComposeLoudnormArgs(joined, filmPath, film.Loudness, &timeline.LoudnessMeasured)); err != nil {
		return nil, err
	}
	timeline.EncodePasses = 1
	info, err := videoComposeOutputInfo(filmPath)
	if err != nil {
		return nil, err
	}
	if info.Size() > film.Delivery.MaxBytes {
		capped := filepath.Join(scratch, "capped.mp4")
		args, err := videoComposeCapArgs(filmPath, capped, timeline.Duration, film.Delivery.MaxBytes)
		if err != nil {
			return nil, err
		}
		if err := run(args); err != nil {
			return nil, err
		}
		info, err = videoComposeOutputInfo(capped)
		if err != nil {
			return nil, err
		}
		if info.Size() > film.Delivery.MaxBytes {
			return nil, fmt.Errorf("one capped encode still exceeds delivery.max_bytes")
		}
		if err := os.Rename(capped, filmPath); err != nil {
			return nil, err
		}
		timeline.EncodePasses++
	}
	finalMedia, err := probe(filmPath)
	if err != nil {
		return nil, err
	}
	// AAC padding and container timebases permit at most one video frame drift.
	fps, _ := videoComposeFPS(reference.FPS)
	if math.Abs(finalMedia.Duration-timeline.Duration) > 1/fps {
		return nil, fmt.Errorf("final film duration disagrees with timeline")
	}
	timeline.Bytes = info.Size()
	posterPath := filepath.Join(outDir, "poster.png")
	if err := removeStaleBuiltinOutput(posterPath); err != nil {
		return nil, err
	}
	if poster != "" {
		if err := run([]string{"-hide_banner", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-i", poster, "-frames:v", "1", posterPath}); err != nil {
			return nil, err
		}
	} else if err := run([]string{"-hide_banner", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-ss", videoComposeNumber(reference.Duration), "-i", filmPath, "-frames:v", "1", posterPath}); err != nil {
		return nil, err
	}
	if _, err := videoComposeOutputInfo(posterPath); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(timeline, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "chapters.json"), raw, 0o600); err != nil {
		return nil, err
	}
	return []string{"film.mp4", "poster.png", "chapters.json"}, nil
}

func videoComposeOutputInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, fmt.Errorf("encoder did not produce a nonempty regular %s", filepath.Base(path))
	}
	return info, nil
}
func videoComposePrepareArgs(input, output string, ref videoComposeMedia, copyStreams bool) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-i", input, "-map", "0:v:0", "-map", "0:a:0", "-map_metadata", "-1", "-map_chapters", "-1"}
	if copyStreams {
		return append(args, "-c", "copy", output)
	}
	return append(args, "-vf", fmt.Sprintf("scale=%d:%d,setsar=1", ref.Width, ref.Height), "-r", ref.FPS, "-c:v", "libx264", "-crf", "17", "-threads", "4", "-preset", "slow", "-tune", "animation", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "192k", "-ar", ref.SampleRate, "-ac", strconv.Itoa(ref.Channels), output)
}
func videoComposeLoudnormArgs(input, output string, target videoComposeLoudness, stats *loudnormStats) []string {
	filter := fmt.Sprintf("loudnorm=I=%s:TP=%s:LRA=%s", videoComposeNumber(target.I), videoComposeNumber(target.TP), videoComposeNumber(target.LRA))
	args := []string{"-hide_banner", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-i", input}
	if stats == nil {
		return append(args, "-vn", "-af", filter+":print_format=json", "-f", "null", "-")
	}
	filter += fmt.Sprintf(":measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true", stats.InputI, stats.InputTP, stats.InputLRA, stats.InputThresh, stats.TargetOffset)
	return append(args, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "copy", "-af", filter, "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-movflags", "+faststart", output)
}
func videoComposeCapArgs(input, output string, duration float64, maxBytes int64) ([]string, error) {
	// Reserve 5% for muxing variance and the copied 192k AAC track. Hard-check
	// actual bytes after the single pass: a maxrate is not an output-size promise.
	if !videoComposeFinite(duration) || duration <= 0 || maxBytes <= 0 {
		return nil, fmt.Errorf("invalid delivery budget")
	}
	videoRate := int64(float64(maxBytes)*8*0.95/duration) - 192000
	if videoRate < 64000 {
		return nil, fmt.Errorf("delivery budget cannot support audio and video")
	}
	rate := strconv.FormatInt(videoRate, 10)
	return []string{"-hide_banner", "-nostdin", "-y", "-protocol_whitelist", "file,pipe", "-i", input, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "libx264", "-b:v", rate, "-maxrate", rate, "-bufsize", strconv.FormatInt(videoRate*2, 10), "-pix_fmt", "yuv420p", "-c:a", "copy", "-movflags", "+faststart", output}, nil
}
