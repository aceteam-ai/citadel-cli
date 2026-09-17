package services

import (
	"os"
	"path/filepath"
	"testing"
)

// readComposeFile reads a compose template by filename from services/compose/,
// mirroring darwin_variants_test.go so these tests run on the linux CI host even
// though the darwin variant is only compiled into a darwin build.
func readComposeFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("compose", name))
	if err != nil {
		t.Fatalf("read compose/%s: %v", name, err)
	}
	return string(raw)
}

func TestComposeDeclaresNvidiaReservation(t *testing.T) {
	// The linux originals reserve an nvidia device; the darwin variants drop it.
	cases := []struct {
		file string
		want bool
	}{
		{"ollama.yml", true},
		{"llamacpp.yml", true},
		{"ollama.darwin.yml", false},
		{"llamacpp.darwin.yml", false},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			if got := composeDeclaresNvidiaReservation(readComposeFile(t, c.file)); got != c.want {
				t.Errorf("composeDeclaresNvidiaReservation(%s) = %v, want %v", c.file, got, c.want)
			}
		})
	}
	// A non-compose / unparseable input reports no reservation (conservative).
	if composeDeclaresNvidiaReservation("::: not yaml :::") {
		t.Error("composeDeclaresNvidiaReservation(garbage) = true, want false")
	}
}

func TestShouldHealStaleGPUReservation(t *testing.T) {
	linuxOllama := readComposeFile(t, "ollama.yml")                           // has nvidia; a known hash
	darwinOllama := readComposeFile(t, "ollama.darwin.yml")                   // no nvidia; the current darwin template
	linuxLlama := readComposeFile(t, "llamacpp.yml")                          // has nvidia; a known hash
	darwinLlama := readComposeFile(t, "llamacpp.darwin.yml")                  // no nvidia; the current darwin template
	handEdited := linuxOllama + "\n# operator note: pinned for our GPU box\n" // nvidia present, hash unknown

	tests := []struct {
		name      string
		onDisk    string
		current   string
		knownHash map[string]bool
		want      bool
	}{
		{
			// The citadel-cli#1069 case: a Mac at v2.150-v2.160 has the current
			// linux ollama.yml (nvidia) on disk; the darwin build's current
			// template dropped it. Heal.
			name:      "stale nvidia ollama -> darwin variant heals",
			onDisk:    linuxOllama,
			current:   darwinOllama,
			knownHash: KnownComposeHashes["ollama"],
			want:      true,
		},
		{
			name:      "stale nvidia llamacpp -> darwin variant heals",
			onDisk:    linuxLlama,
			current:   darwinLlama,
			knownHash: KnownComposeHashes["llamacpp"],
			want:      true,
		},
		{
			// A hand-edited file still reserving nvidia is NOT citadel-written
			// (hash absent from the known set) and must be preserved.
			name:      "hand-edited nvidia file is preserved",
			onDisk:    handEdited,
			current:   darwinOllama,
			knownHash: KnownComposeHashes["ollama"],
			want:      false,
		},
		{
			// On a NON-darwin build the current template still reserves nvidia, so
			// the heal short-circuits regardless of on-disk content -> linux/windows
			// byte-identical. current != onDisk here proves it is the current-has-
			// nvidia guard, not the already-current guard.
			name:      "current still reserves nvidia (non-darwin build) -> no heal",
			onDisk:    linuxOllama,
			current:   linuxLlama,
			knownHash: KnownComposeHashes["ollama"],
			want:      false,
		},
		{
			// Already the darwin variant: no nvidia on disk, nothing to heal.
			name:      "already current darwin variant -> no heal",
			onDisk:    darwinOllama,
			current:   darwinOllama,
			knownHash: KnownComposeHashes["ollama"],
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldHealStaleGPUReservation(tt.onDisk, tt.current, tt.knownHash); got != tt.want {
				t.Errorf("ShouldHealStaleGPUReservation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHealStaleGPUReservationOnDisk(t *testing.T) {
	linuxOllama := readComposeFile(t, "ollama.yml")
	darwinOllama := readComposeFile(t, "ollama.darwin.yml")
	known := KnownComposeHashes["ollama"]

	t.Run("rewrites a stale nvidia file, then is idempotent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ollama.yml")
		if err := os.WriteFile(path, []byte(linuxOllama), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}

		healed, err := HealStaleGPUReservationOnDisk(path, darwinOllama, known)
		if err != nil {
			t.Fatalf("heal: %v", err)
		}
		if !healed {
			t.Fatal("expected the stale file to be healed")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != darwinOllama {
			t.Errorf("healed file content = %q, want the darwin variant", string(got))
		}

		// Second call: already current -> no rewrite.
		healed, err = HealStaleGPUReservationOnDisk(path, darwinOllama, known)
		if err != nil {
			t.Fatalf("heal (2nd): %v", err)
		}
		if healed {
			t.Error("second heal reported a rewrite; expected no-op on an already-current file")
		}
	})

	t.Run("preserves a hand-edited file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ollama.yml")
		handEdited := linuxOllama + "\n# operator note\n"
		if err := os.WriteFile(path, []byte(handEdited), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		healed, err := HealStaleGPUReservationOnDisk(path, darwinOllama, known)
		if err != nil {
			t.Fatalf("heal: %v", err)
		}
		if healed {
			t.Error("hand-edited file was rewritten; it must be preserved")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != handEdited {
			t.Error("hand-edited file content changed")
		}
	})

	t.Run("missing file is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "does-not-exist.yml")
		healed, err := HealStaleGPUReservationOnDisk(path, darwinOllama, known)
		if err != nil {
			t.Fatalf("heal: %v", err)
		}
		if healed {
			t.Error("missing file reported a rewrite")
		}
	})
}
