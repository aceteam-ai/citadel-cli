package cmd

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVideoComposeDurationAccumulatorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, part float64
		want        float64
		wantOK      bool
	}{
		{name: "first", part: 4, want: 4, wantOK: true},
		{name: "exact total", total: 599.5, part: 0.5, want: 600, wantOK: true},
		{name: "one beyond", total: 599.5, part: 0.501},
		{name: "next float beyond", part: math.Nextafter(600, math.Inf(1))},
		{name: "tiny part after exact", total: 600, part: math.SmallestNonzeroFloat64},
		{name: "overflow", total: math.MaxFloat64, part: math.MaxFloat64},
		{name: "nan total", total: math.NaN(), part: 1},
		{name: "nan part", part: math.NaN()},
		{name: "infinite total", total: math.Inf(1), part: 1},
		{name: "infinite part", part: math.Inf(1)},
		{name: "negative total", total: -1, part: 1},
		{name: "negative part", part: -1},
		{name: "zero part"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := videoComposeAddDuration(tc.total, tc.part)
			if (err == nil) != tc.wantOK || tc.wantOK && got != tc.want {
				t.Fatalf("duration = %v, %v; want %v success=%v", got, err, tc.want, tc.wantOK)
			}
		})
	}
	for _, tc := range []struct {
		actual, timeline, fps float64
		wantOK                bool
	}{
		{600, 600, 30, true},
		{599.99, 600, 30, true},
		{600.001, 600, 30, false}, // Frame tolerance never extends the hard cap.
		{599.95, 600, 30, false},
		{1, math.Inf(1), 30, false},
		{1, 1, 0, false},
	} {
		if err := videoComposeCheckOutputDuration(tc.actual, tc.timeline, tc.fps); (err == nil) != tc.wantOK {
			t.Fatalf("output duration %+v: %v", tc, err)
		}
	}
}

func TestVideoComposeCardsAloneRefuseBeforeToolDiscovery(t *testing.T) {
	raw := videoComposeTestFilm(t, func(value map[string]any) {
		value["intro"].(map[string]any)["seconds"] = 60
		value["outro"].(map[string]any)["seconds"] = 60
		chapters := make([]any, 9)
		for index := range chapters {
			chapter := map[string]any{}
			for key, field := range videoComposeTestChapter(value) {
				chapter[key] = field
			}
			chapter["file"] = "chapter-" + string(rune('a'+index)) + ".mp4"
			chapter["card_seconds"] = 60
			chapters[index] = chapter
		}
		value["chapters"] = chapters // 660 seconds, before any chapter duration.
	})
	inputs := videoComposeTestInputs(t, raw)
	fake := &videoComposeFake{t: t, media: map[string]videoComposeMedia{}}
	deps := fake.deps()
	deps.look = func(string) (string, error) { t.Fatal("overlong title cards reached tool discovery"); return "", nil }
	deps.run = func(context.Context, string, []string) (videoComposeCommandOutput, error) {
		t.Fatal("overlong title cards invoked media")
		return videoComposeCommandOutput{}, nil
	}
	outDir := t.TempDir()
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, deps); err == nil || !strings.Contains(err.Error(), "600 seconds") {
		t.Fatalf("card-only budget was not refused: %v", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 0 || fake.rendered != 0 {
		t.Fatal("overlong title cards created outputs or reached renderer")
	}
}

func TestVideoComposeProjectedTotalIncludesEveryChapterAndCard(t *testing.T) {
	for _, secondDuration := range []float64{288.5, 288.501} {
		t.Run(videoComposeNumber(secondDuration), func(t *testing.T) {
			raw := videoComposeTestFilm(t, func(value map[string]any) {
				chapter := map[string]any{}
				for key, field := range videoComposeTestChapter(value) {
					chapter[key] = field
				}
				chapter["file"] = "second.mp4"
				chapter["card_seconds"] = 1.5
				value["chapters"] = append(value["chapters"].([]any), chapter)
			})
			inputs := videoComposeTestInputs(t, raw)
			second := filepath.Join(filepath.Dir(inputs[0]), "second.mp4")
			if err := os.WriteFile(second, videoComposeTestMP4(true), 0o600); err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, second)
			fake := &videoComposeFake{t: t, media: map[string]videoComposeMedia{
				inputs[1]: videoComposeTestMedia(298.5), second: videoComposeTestMedia(secondDuration),
			}, encodedBytes: 3}
			outDir := t.TempDir()
			_, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, fake.deps())
			// Intro/outro=9, chapter cards=4, clips=298.5+288.5: exactly 600.
			if secondDuration == 288.5 {
				if err != nil {
					t.Fatal(err)
				}
				var timeline videoComposeTimeline
				body, readErr := os.ReadFile(filepath.Join(outDir, "chapters.json"))
				if readErr != nil || json.Unmarshal(body, &timeline) != nil || timeline.Duration != 600 || len(timeline.Chapters) != 2 || timeline.Chapters[1].End != 595 {
					t.Fatal("exact-600 timeline lost chapter/card accounting")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "600 seconds") || fake.rendered != 0 || len(fake.ffmpegCalls()) != 0 {
					t.Fatalf("overlong projected total reached rendering/encoding: %v", err)
				}
				entries, readErr := os.ReadDir(outDir)
				if readErr != nil || len(entries) != 0 {
					t.Fatal("overlong projected total created scratch or outputs")
				}
			}
		})
	}
}

type videoDurationVisuals struct {
	base *videoComposeFake
	edit func(int, string)
}

func (visuals videoDurationVisuals) RenderCard(ctx context.Context, film videoComposeFilm, index int, ref videoComposeMedia, path string) error {
	if err := visuals.base.RenderCard(ctx, film, index, ref, path); err != nil {
		return err
	}
	visuals.edit(index, path)
	return nil
}

func (visuals videoDurationVisuals) BurnDisclosure(ctx context.Context, chapter videoComposeChapter, ref videoComposeMedia, input, output string) error {
	return visuals.base.BurnDisclosure(ctx, chapter, ref, input, output)
}

func TestVideoComposeActualDurationRecheckedThroughEveryOutputStage(t *testing.T) {
	for _, stage := range []string{"intro", "card", "clip", "outro", "joined", "normalized", "capped"} {
		t.Run(stage, func(t *testing.T) {
			inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
			outDir := t.TempDir()
			fake := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(588.5)}, encodedBytes: 3}
			if stage == "normalized" || stage == "capped" {
				fake.encodedBytes, fake.cappedBytes = videoComposeDefaultMaxBytes+1, videoComposeDefaultMaxBytes
			}
			deps := fake.deps()
			injected := false
			deps.visuals = videoDurationVisuals{base: fake, edit: func(index int, path string) {
				media := fake.media[path]
				switch {
				case stage == "intro" && index == -1:
					media.Duration = 601
					injected = true
				case stage == "card" && index == 0:
					media.Duration = 597
					injected = true
				case stage == "outro" && index == 1:
					media.Duration = 5.001
					injected = true
				}
				fake.media[path] = media
			}}
			capped := false
			deps.run = func(ctx context.Context, bin string, args []string) (videoComposeCommandOutput, error) {
				last := args[len(args)-1]
				if bin == "ffprobe" && filepath.Base(last) == "film.mp4" && stage == "capped" && capped {
					// The fake media map cannot follow os.Rename. Verify the actual
					// renamed file, then supply its distinct capped probe result.
					info, err := os.Stat(last)
					if err != nil || info.Size() != fake.cappedBytes {
						t.Fatal("final cap probe did not inspect the renamed capped output")
					}
					injected = true
					return videoComposeCommandOutput{stdout: videoComposeTestProbe(videoComposeTestMedia(600.001))}, nil
				}
				output, err := fake.run(ctx, bin, args)
				if err != nil || bin != "ffmpeg" {
					return output, err
				}
				media := fake.media[last]
				switch {
				case stage == "clip" && filepath.Base(last) == "clip-00.mp4":
					media.Duration = 594
					injected = true
				case stage == "joined" && filepath.Base(last) == "joined.mp4":
					media.Duration = 600.001
					injected = true
				case stage == "normalized" && filepath.Base(last) == "film.mp4":
					media.Duration = 600.001
					injected = true
				}
				fake.media[last] = media
				if filepath.Base(last) == "capped.mp4" {
					capped = true
				}
				return output, nil
			}
			if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, outDir, deps); err == nil || !strings.Contains(err.Error(), "600 seconds") {
				t.Fatalf("actual %s duration crossed hard total cap: %v", stage, err)
			}
			if !injected {
				t.Fatal("duration refusal occurred before the targeted actual stage")
			}
			if _, err := os.Stat(filepath.Join(outDir, "chapters.json")); !os.IsNotExist(err) {
				t.Fatal("overlong output published a success timeline")
			}
			if _, err := os.Stat(filepath.Join(outDir, "poster.png")); !os.IsNotExist(err) {
				t.Fatal("overlong output reached poster generation")
			}
			if stage == "intro" && fake.rendered != 1 || (stage == "card" || stage == "clip") && fake.rendered != 2 {
				t.Fatal("actual duration refusal rendered a later title card")
			}
			for _, call := range fake.ffmpegCalls() {
				joined := strings.Join(call, " ")
				if (stage == "intro" || stage == "card") || (stage == "clip" || stage == "outro") && strings.Contains(joined, "-f concat") ||
					stage == "joined" && strings.Contains(joined, "loudnorm=") || stage == "normalized" && strings.Contains(joined, "-maxrate") {
					t.Fatalf("actual %s refusal reached a later encoding stage: %v", stage, call)
				}
			}
			if stage == "capped" && !capped {
				t.Fatal("capped-duration regression never exercised the cap path")
			}
		})
	}
}
