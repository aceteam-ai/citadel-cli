package nodesession

import (
	"os"
	"testing"
)

func TestEnrollmentDefaultsAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device bool
		want   Mode
	}{
		{"authkey", false, Presence},
		{"device auth", true, Worker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, err := LoadOrInitialize(dir, tc.device)
			if err != nil || cfg.Mode != tc.want {
				t.Fatalf("initial mode = %+v, %v; want %s", cfg, err, tc.want)
			}
			// Credentials can change later without silently changing intent.
			again, err := LoadOrInitialize(dir, !tc.device)
			if err != nil || again.Mode != tc.want {
				t.Fatalf("persisted mode = %+v, %v; want %s", again, err, tc.want)
			}
			info, err := os.Stat(Path(dir))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("session config permissions = %v, %v", info, err)
			}
		})
	}
}

func TestCorruptOrUnknownModeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("mode: surprise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrInitialize(dir, true); err == nil {
		t.Fatal("unknown mode enabled a worker")
	}
	if err := os.WriteFile(Path(dir), []byte("mode: [bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrInitialize(dir, true); err == nil {
		t.Fatal("malformed config enabled a worker")
	}
}
