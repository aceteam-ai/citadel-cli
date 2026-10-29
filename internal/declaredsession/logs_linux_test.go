//go:build linux

package declaredsession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogsBoundedRotationAndEvents(t *testing.T) {
	_, session := fixtureStore(t)
	logs, err := session.OpenLogs(LogPolicy{RotateBytes: 4096, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(session.Paths().Dir, string(ConsoleLog))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening log policy created output")
	}
	raw := bytes.Repeat([]byte("x"), 5*4096+17)
	if err := logs.AppendConsole(raw); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"console.log", "console.log.1", "console.log.2"} {
		info, err := os.Stat(filepath.Join(session.Paths().Dir, name))
		if err != nil || info.Size() > 4096 || info.Mode().Perm() != 0600 {
			t.Fatalf("retained log exceeded policy: %s err=%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(session.Paths().Dir, "console.log.3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("extra rotated archive retained")
	}
	tail, err := session.ReadLogTail(ConsoleLog, 8)
	if err != nil || string(tail) != "xxxxxxxx" {
		t.Fatalf("bounded tail: %q %v", tail, err)
	}
	event := Event{Kind: "spawn", At: time.Unix(1, 0), Incarnation: strings.Repeat("a", 32), ResolvedExecutable: "/bin/fixture", EnvironmentHash: digest([]byte("synthetic"))}
	if err := logs.AppendEvent(event); err != nil {
		t.Fatal(err)
	}
	event.Reason = "untrusted raw error or secret"
	if err := logs.AppendEvent(event); !errors.Is(err, ErrUnsafe) {
		t.Fatal("free-text diagnostic accepted in event log")
	}
	if _, err := session.ReadLogTail(LogKind("../outside"), 8); !errors.Is(err, ErrConfig) {
		t.Fatal("log tail accepted an arbitrary path")
	}
}
