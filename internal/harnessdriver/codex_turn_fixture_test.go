package harnessdriver

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func TestCodexOfflineGoldenSuppliedStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-turn.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/codex-turn-events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	r := offlineTurn(t)
	var output bytes.Buffer
	w := NewFrameWriter(&output)
	err = RunStreams(context.Background(), io.NopCloser(bytes.NewReader(raw)), io.NopCloser(bytes.NewReader(nil)), func(_ context.Context, line []byte) error {
		got, err := r.Apply(line, projectionTime)
		if err != nil {
			return err
		}
		for _, e := range got.Events {
			if err = e.Validate(); err != nil {
				return err
			}
			if err = w.WriteJSON(e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("golden projection changed:\n%s", output.Bytes())
	}
}
