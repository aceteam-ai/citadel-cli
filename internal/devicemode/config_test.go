package devicemode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.json")
	in := &Config{
		NodeUID:     "uid-123",
		NexusURL:    "https://nexus.example",
		ReenrollURL: "https://nexus.example/fabric/reenroll",
		APIBaseURL:  "https://aceteam.example",
	}
	if err := saveConfigTo(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadConfigFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadConfigMissingIsNotExist(t *testing.T) {
	_, err := loadConfigFrom(filepath.Join(t.TempDir(), "nope.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestLoadConfigFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.json")
	if err := os.WriteFile(path, []byte(`{"node_uid": "uid-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NexusURL != DefaultNexusURL || cfg.ReenrollURL != DefaultReenrollURL {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestMachineIDStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine-id")
	first, err := machineIDAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("machine id length = %d, want 32 hex chars", len(first))
	}
	second, err := machineIDAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("machine id not stable: %q != %q", first, second)
	}
}

func TestPairingCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := NewPairingCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q length != 6", code)
		}
		for _, c := range code {
			if !((c >= 'A' && c <= 'Z') || (c >= '2' && c <= '9')) {
				t.Fatalf("code %q contains invalid char %q", code, c)
			}
		}
		seen[code] = true
	}
	if len(seen) < 40 {
		t.Fatalf("codes look non-random: %d unique of 50", len(seen))
	}
}

func TestPairURL(t *testing.T) {
	got := PairURL("https://aceteam.ai", "ABC234")
	if got != "https://aceteam.ai/fabric/pair?code=ABC234" {
		t.Fatalf("got %q", got)
	}
}
