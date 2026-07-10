package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFileArgs(t *testing.T) {
	dir := t.TempDir()

	composePath := filepath.Join(dir, "livekit.yml")
	envPath := filepath.Join(dir, "livekit.env")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("LIVEKIT_API_KEY=k\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got := EnvFileArgs(composePath)
	want := []string{"--env-file", envPath}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("EnvFileArgs = %v, want %v", got, want)
	}

	// No sibling -> nil, so call sites can append unconditionally.
	bare := filepath.Join(dir, "ollama.yml")
	if err := os.WriteFile(bare, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := EnvFileArgs(bare); got != nil {
		t.Errorf("EnvFileArgs without sibling = %v, want nil", got)
	}

	if got := EnvFileArgs(""); got != nil {
		t.Errorf("EnvFileArgs(\"\") = %v, want nil", got)
	}

	// A directory named like the env sibling must not be passed as --env-file.
	dirCompose := filepath.Join(dir, "weird.yml")
	if err := os.WriteFile(dirCompose, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "weird.env"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := EnvFileArgs(dirCompose); got != nil {
		t.Errorf("EnvFileArgs with dir sibling = %v, want nil", got)
	}
}
