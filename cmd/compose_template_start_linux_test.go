//go:build linux

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// startService on a running TEI whose compose is the pre-pin citadel template
// refreshes the file and runs compose up exactly once; the next start takes
// the already-running shortcut. A failed up restores the pre-pin file. The
// container engine is a fake script on an isolated PATH that reports the
// container as running and records its arguments.
func TestStartServiceAppliesRefreshedTEITemplateOnce(t *testing.T) {
	old := prePinTEICompose(t)
	for _, tc := range []struct {
		name     string
		fail     bool
		starts   int
		wantUps  int
		wantFile string
	}{
		{name: "refresh applied once", starts: 2, wantUps: 1, wantFile: services.ServiceMap["tei"]},
		{name: "failed up restores file", fail: true, starts: 1, wantUps: 1, wantFile: old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path := writeNodeFixture(t, "tei", old, "")
			bin := t.TempDir()
			logPath := filepath.Join(bin, "calls.log")
			composeExit := "0"
			if tc.fail {
				composeExit = "1"
			}
			script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\ncase \"$1\" in\n  inspect) echo running ;;\n  compose) exit %s ;;\nesac\nexit 0\n", logPath, composeExit)
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
			t.Setenv("CITADEL_NODE_DIR", "")
			t.Setenv("CITADEL_WORKSPACE", t.TempDir())
			t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "")
			for i := 0; i < tc.starts; i++ {
				err := startService("tei", path)
				if (err != nil) != tc.fail {
					t.Fatalf("start %d: %v (want failure=%v)", i, err, tc.fail)
				}
			}
			calls, _ := os.ReadFile(logPath)
			ups := 0
			for _, line := range strings.Split(string(calls), "\n") {
				if strings.HasPrefix(line, "compose ") && strings.Contains(line, " up -d") {
					ups++
					if !strings.Contains(line, path) || strings.Contains(line, "--force-recreate") {
						t.Fatalf("compose up = %q, want an unforced up from %s", line, path)
					}
				}
			}
			if ups != tc.wantUps {
				t.Fatalf("compose ups = %d, want %d; calls:\n%s", ups, tc.wantUps, calls)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.wantFile {
				t.Fatalf("compose after start:\n%s", got)
			}
		})
	}
}
