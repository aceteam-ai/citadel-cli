package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestLocalStartOpensViewerWithoutLeakingTheCode(t *testing.T) {
	ownerDir := t.TempDir()
	var mu sync.Mutex
	var opened []string
	rt := newLocalCobrowseRuntime(localCobrowseOptions{
		ownerDir: func() string { return ownerDir },
		browser:  newCmdFakeBrowser(),
		policy:   allowAllURLs,
		viewer:   true,
		openURL:  func(u string) error { mu.Lock(); opened = append(opened, u); mu.Unlock(); return nil },
	})
	defer rt.close()

	resp := runLocalBridge(t, rt, nil, call(1, "local_cobrowse_start", map[string]any{"goal": "demo"}))
	content, isErr := toolResult(t, resp["1"])
	text := firstText(content)
	if isErr || !strings.Contains(text, "Citadel viewer") {
		t.Fatalf("start: %v", resp["1"])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 1 || !strings.HasSuffix(opened[0], ".html") || strings.Contains(opened[0], "code=") {
		t.Fatalf("start should open the viewer through a launch file, opened %v", opened)
	}
	launch, err := os.ReadFile(opened[0])
	if err != nil || !strings.Contains(string(launch), "http://127.0.0.1:") {
		t.Fatalf("launch file: %v", err)
	}
	code := string(launch)[strings.Index(string(launch), "code=")+5:]
	code = code[:strings.IndexByte(code, '"')]
	if strings.Contains(text, code) || strings.Contains(text, "/open?") {
		t.Fatal("the one-time viewer code must never be returned over MCP")
	}

	// The viewer port is refused by the address filter.
	ports := rt.viewerPorts()
	if len(ports) != 1 || ports[0] == 0 {
		t.Fatalf("viewer ports: %v", ports)
	}

	// `citadel local viewer` asks the running owner for a fresh link.
	var out bytes.Buffer
	if err := runLocalViewer(context.Background(), ownerDir, true, nil, &out); err != nil {
		t.Fatalf("local viewer: %v", err)
	}
	if !strings.HasPrefix(out.String(), "http://127.0.0.1:") || strings.Contains(out.String(), code) {
		t.Fatalf("local viewer link: %q", out.String())
	}
}

func TestLocalViewerWithoutOwnerExplains(t *testing.T) {
	var out bytes.Buffer
	err := runLocalViewer(context.Background(), t.TempDir(), true, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "start a session from your agent") {
		t.Fatalf("want a plain explanation, got %v", err)
	}
}
