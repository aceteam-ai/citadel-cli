//go:build linux

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose/composefake"
	"github.com/aceteam-ai/citadel-cli/services"
)

// startService on a running TEI whose compose is the pre-pin citadel template
// refreshes the file and replaces the container once; the next start takes the
// already-running shortcut. A failed replacement or preflight leaves the old
// container running under its own name and restores the file. startService's
// own probes see a fake script on an isolated PATH that reports the container
// running; the replacement runs against the composefake engine.
func TestStartServiceAppliesRefreshedTEITemplateOnce(t *testing.T) {
	old := prePinTEICompose(t)
	for _, tc := range []struct {
		name     string
		fail     string // composefake failure step
		starts   int
		wantUps  int
		wantFile string
	}{
		{name: "refresh applied once", starts: 2, wantUps: 1, wantFile: services.ServiceMap["tei"]},
		{name: "failed replacement keeps old container", fail: "up-start", starts: 1, wantUps: 1, wantFile: old},
		{name: "preflight failure touches nothing", fail: "config", starts: 1, wantFile: old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeManagedCompose{}
			installFakeManagedCompose(t, f)
			_, path := writeNodeFixture(t, "tei", old, "")
			prev := f.addRunning("citadel-tei", path)
			if tc.fail != "" {
				f.Fail[tc.fail] = 1
			}
			bin := t.TempDir()
			script := "#!/bin/sh\ncase \"$1\" in\n  inspect) echo running ;;\nesac\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
			for i := 0; i < tc.starts; i++ {
				err := startService("tei", path)
				if (err != nil) != (tc.fail != "") {
					t.Fatalf("start %d: %v (want failure=%v)", i, err, tc.fail != "")
				}
			}
			if f.ups() != tc.wantUps {
				t.Fatalf("compose ups = %d, want %d: %v", f.ups(), tc.wantUps, f.Calls)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.wantFile {
				t.Fatalf("compose after start:\n%s", got)
			}
			all := f.Containers()
			if len(all) != 1 || all[0].Name != "citadel-tei" || all[0].State != "running" {
				t.Fatalf("containers = %+v; want exactly one running citadel-tei", all)
			}
			if tc.fail != "" {
				if all[0] != prev {
					t.Fatalf("old container not intact after failure: %+v, was %+v", all[0], prev)
				}
				if tc.fail == "config" && (f.Count("stop") != 0 || f.Count("rename") != 0) {
					t.Fatalf("preflight failure touched the container: %v", f.Calls)
				}
				return
			}
			if all[0].ID == prev.ID || all[0].Config != composefake.FileHash(path) || f.Count("rm", "-f", prev.ID) != 1 {
				t.Fatalf("replacement = %+v; want a new container and the backup removed: %v", all[0], fmt.Sprint(f.Calls))
			}
		})
	}
}
