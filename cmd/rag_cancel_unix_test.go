//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
	"github.com/spf13/cobra"
)

const ragSIGINTHelperEnv = "CITADEL_TEST_RAG_SIGINT_HELPER"

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestRunRAGIndexProgrammaticCancellationRemainsHard(t *testing.T) {
	owned := t.TempDir()
	workspace := filepath.Join(owned, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "a.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CITADEL_WORKSPACE", workspace)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(owned, "index.db"))
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := &cobra.Command{}
	command.SetContext(ctx)
	oldJSON, oldPattern := ragJSON, ragFilePattern
	ragJSON = true
	ragFilePattern = ""
	t.Cleanup(func() { ragJSON, ragFilePattern = oldJSON, oldPattern })
	err := runRAGIndex(command, []string{workspace})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}

func TestRAGIndexSIGINTHelper(t *testing.T) {
	if os.Getenv(ragSIGINTHelperEnv) != "1" {
		t.Skip("owned subprocess helper")
	}
	ragJSON = true
	ragFilePattern = ""
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runRAGIndex(cmd, []string{os.Getenv("CITADEL_WORKSPACE")}); err != nil {
		t.Fatalf("runRAGIndex: %v", err)
	}
}

func TestRAGIndexOwnedChildSIGINTCommitsCurrentFile(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = io.Copy(io.Discard, r.Body)
		startedOnce.Do(func() { close(started) })
		<-release
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "gte"})
	})
	tei := httptest.NewServer(mux)
	defer tei.Close()
	defer releaseOnce.Do(func() { close(release) })

	owned := t.TempDir()
	workspace := filepath.Join(owned, "node", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(owned, "node", "index.db")
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, testBin, "-test.run=^TestRAGIndexSIGINTHelper$")
	child.Dir = owned
	child.Env = []string{
		ragSIGINTHelperEnv + "=1",
		"HOME=" + filepath.Join(owned, "home"),
		"XDG_CACHE_HOME=" + filepath.Join(owned, "xdg-cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(owned, "xdg-config"),
		"XDG_DATA_HOME=" + filepath.Join(owned, "xdg-data"),
		"TMPDIR=" + filepath.Join(owned, "tmp"),
		"CITADEL_WORKSPACE=" + workspace,
		"CITADEL_TEI_URL=" + tei.URL,
	}
	for _, dir := range []string{"home", "xdg-cache", "xdg-config", "xdg-data", "tmp"} {
		if err := os.MkdirAll(filepath.Join(owned, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var output lockedBuffer
	child.Stdout = &output
	child.Stderr = &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	joined := false
	var waitErr error
	join := func(kill bool) error {
		if joined {
			return waitErr
		}
		if kill && child.Process != nil {
			_ = child.Process.Kill()
		}
		select {
		case waitErr = <-done:
			joined = true
			return waitErr
		case <-time.After(2 * time.Second):
			return errors.New("owned child Wait did not join")
		}
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !joined {
			_ = join(true)
		}
	})
	select {
	case <-started:
	case <-ctx.Done():
		releaseOnce.Do(func() { close(release) })
		_ = join(true)
		t.Fatalf("owned child never reached embedding request: %v\n%s", ctx.Err(), output.String())
	}
	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal owned child: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		joined = true
		waitErr = err
		if err != nil {
			t.Fatalf("owned child exit: %v\n%s", err, output.String())
		}
	case <-ctx.Done():
		_ = join(true)
		t.Fatalf("owned child did not exit: %v\n%s", ctx.Err(), output.String())
	}
	if !strings.Contains(output.String(), `"status": "cancelled"`) || !strings.Contains(output.String(), `"checkpoint": "partial"`) {
		t.Fatalf("child output missing cancelled partial result:\n%s", output.String())
	}
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, indexed, err := store.FileHash(filepath.Join(workspace, "a.md")); err != nil || !indexed {
		t.Fatalf("a.md indexed/error=%v/%v, want true/nil", indexed, err)
	}
	if _, indexed, err := store.FileHash(filepath.Join(workspace, "b.md")); err != nil || indexed {
		t.Fatalf("b.md indexed/error=%v/%v, want false/nil", indexed, err)
	}
}
