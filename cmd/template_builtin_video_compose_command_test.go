package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestVideoComposeCommandStdoutBoundAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &videoComposeOutputCapture{limit: videoComposeMaxStdout, cancel: cancel}
	exact := bytes.Repeat([]byte("a"), videoComposeMaxStdout)
	if n, err := stdout.Write(exact); err != nil || n != len(exact) || stdout.overflow || ctx.Err() != nil {
		t.Fatalf("exact stdout cap: n=%d err=%v overflow=%v ctx=%v", n, err, stdout.overflow, ctx.Err())
	}
	if n, err := stdout.Write([]byte("b")); err != nil || n != 1 || !stdout.overflow || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("overflow did not cancel immediately: n=%d err=%v overflow=%v ctx=%v", n, err, stdout.overflow, ctx.Err())
	}
	if n, err := stdout.Write(exact); err != nil || n != len(exact) || !bytes.Equal(stdout.data, exact) {
		t.Fatalf("overflow capture grew or stopped consuming: n=%d err=%v len=%d", n, err, len(stdout.data))
	}
	// Even a successful exit racing cancellation must not parse truncated JSON.
	if _, err := finishVideoComposeCommand(stdout, &videoComposeOutputCapture{}, nil, nil); !errors.Is(err, errVideoComposeStdoutLimit) {
		t.Fatalf("successful exit hid sticky overflow: %v", err)
	}
	runErr, cleanupErr := errors.New("run refused"), errors.New("scope cleanup refused")
	if _, err := finishVideoComposeCommand(stdout, &videoComposeOutputCapture{}, runErr, cleanupErr); !errors.Is(err, runErr) || !errors.Is(err, cleanupErr) || !errors.Is(err, errVideoComposeStdoutLimit) {
		t.Fatalf("command custody lost an error: %v", err)
	}
}

func TestVideoComposeCommandStderrTailBound(t *testing.T) {
	stderr := &videoComposeOutputCapture{limit: videoComposeMaxStderr, tail: true}
	huge := bytes.Repeat([]byte("x"), 4*videoComposeMaxStderr)
	if n, err := stderr.Write(huge); err != nil || n != len(huge) || len(stderr.data) != videoComposeMaxStderr || cap(stderr.data) > 2*videoComposeMaxStderr {
		t.Fatalf("large one-shot tail: n=%d err=%v len=%d cap=%d", n, err, len(stderr.data), cap(stderr.data))
	}
	for _, p := range []string{"last", " warning"} {
		if n, err := stderr.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("tail did not consume write: %d %v", n, err)
		}
	}
	if len(stderr.data) != videoComposeMaxStderr || !strings.HasSuffix(string(stderr.data), "last warning") || stderr.overflow {
		t.Fatalf("tail rollover: len=%d overflow=%v", len(stderr.data), stderr.overflow)
	}
	stdout := &videoComposeOutputCapture{data: []byte("separate stdout")}
	out, err := finishVideoComposeCommand(stdout, stderr, nil, nil)
	if err != nil || string(out.stdout) != "separate stdout" || !bytes.Equal(out.stderr, stderr.data) {
		t.Fatalf("separated command result: %+v %v", out, err)
	}
}

func TestVideoComposeProbeDoesNotParseStderrJSON(t *testing.T) {
	inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
	f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10)}}
	deps := f.deps()
	deps.run = func(context.Context, string, []string) (videoComposeCommandOutput, error) {
		return videoComposeCommandOutput{stdout: []byte("malformed stdout"), stderr: videoComposeTestProbe(videoComposeTestMedia(10))}, nil
	}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, t.TempDir(), deps); err == nil || f.rendered != 0 {
		t.Fatalf("probe accepted stderr metadata or rendered before refusal: %v rendered=%d", err, f.rendered)
	}
}

func TestVideoComposeDoesNotParseFailedCommand(t *testing.T) {
	inputs := videoComposeTestInputs(t, []byte(validVideoComposeFilm))
	f := &videoComposeFake{t: t, media: map[string]videoComposeMedia{inputs[1]: videoComposeTestMedia(10)}}
	deps := f.deps()
	deps.run = func(context.Context, string, []string) (videoComposeCommandOutput, error) {
		return videoComposeCommandOutput{stdout: videoComposeTestProbe(videoComposeTestMedia(10))}, errVideoComposeStdoutLimit
	}
	if _, err := runVideoComposeBuiltin(context.Background(), json.RawMessage(`{}`), inputs, t.TempDir(), deps); !errors.Is(err, errVideoComposeStdoutLimit) || f.rendered != 0 {
		t.Fatalf("failed probe was parsed: %v rendered=%d", err, f.rendered)
	}
}

func TestVideoComposeSafeBasenameSeparators(t *testing.T) {
	for _, name := range []string{"chapter.mp4", "a-b_c.1.mp4"} {
		if !videoComposeSafeBasename(name) {
			t.Fatalf("safe basename refused: %q", name)
		}
	}
	for _, name := range []string{"a/b.mp4", `a\b.mp4`} {
		if videoComposeSafeBasename(name) {
			t.Fatalf("separator accepted in basename: %q", name)
		}
	}
}
