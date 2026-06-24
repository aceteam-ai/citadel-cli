//go:build linux

package power

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSupply creates a fake /sys/class/power_supply/<name> entry.
func writeSupply(t *testing.T, base, name, typ, online string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if typ != "" {
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte(typ+"\n"), 0644); err != nil {
			t.Fatalf("write type: %v", err)
		}
	}
	if online != "" {
		if err := os.WriteFile(filepath.Join(dir, "online"), []byte(online+"\n"), 0644); err != nil {
			t.Fatalf("write online: %v", err)
		}
	}
}

func TestDetectPowerSourceAt(t *testing.T) {
	t.Run("ac online", func(t *testing.T) {
		base := t.TempDir()
		writeSupply(t, base, "AC", "Mains", "1")
		writeSupply(t, base, "BAT0", "Battery", "")
		if got := detectPowerSourceAt(base); got != SourceAC {
			t.Errorf("got %v, want AC", got)
		}
	})

	t.Run("ac offline means battery", func(t *testing.T) {
		base := t.TempDir()
		writeSupply(t, base, "AC", "Mains", "0")
		if got := detectPowerSourceAt(base); got != SourceBattery {
			t.Errorf("got %v, want battery", got)
		}
	})

	t.Run("no ac supply is unknown", func(t *testing.T) {
		base := t.TempDir()
		writeSupply(t, base, "BAT0", "Battery", "")
		if got := detectPowerSourceAt(base); got != SourceUnknown {
			t.Errorf("got %v, want unknown", got)
		}
	})

	t.Run("missing base dir is unknown", func(t *testing.T) {
		if got := detectPowerSourceAt(filepath.Join(t.TempDir(), "nope")); got != SourceUnknown {
			t.Errorf("got %v, want unknown", got)
		}
	})

	t.Run("name-prefix fallback when no type file", func(t *testing.T) {
		base := t.TempDir()
		writeSupply(t, base, "ADP1", "", "1")
		if got := detectPowerSourceAt(base); got != SourceAC {
			t.Errorf("got %v, want AC", got)
		}
	})
}
