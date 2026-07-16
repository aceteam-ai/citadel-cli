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

func TestSiblingEnvPath(t *testing.T) {
	if got := SiblingEnvPath("/etc/citadel/services/vllm.yml"); got != "/etc/citadel/services/vllm.env" {
		t.Errorf("SiblingEnvPath = %q, want /etc/citadel/services/vllm.env", got)
	}
}

// TestUpsertEnvVar covers the SERVICE_START model persistence (#530): create
// the file when absent, replace an existing definition in place, and preserve
// every unrelated line (comments, other keys) untouched.
func TestUpsertEnvVar(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "services", "vllm.env")

	// Create: parent dir + file must be created, 0600.
	if err := UpsertEnvVar(envPath, "VLLM_MODEL", "Qwen/Qwen2.5-0.5B-Instruct"); err != nil {
		t.Fatalf("UpsertEnvVar create: %v", err)
	}
	if v, ok := ReadEnvVar(envPath, "VLLM_MODEL"); !ok || v != "Qwen/Qwen2.5-0.5B-Instruct" {
		t.Errorf("after create: VLLM_MODEL = %q, %v", v, ok)
	}
	if fi, err := os.Stat(envPath); err != nil {
		t.Fatalf("stat env file: %v", err)
	} else if fi.Mode().Perm() != 0600 {
		t.Errorf("env file mode = %o, want 0600", fi.Mode().Perm())
	}

	// Update in place, preserving unrelated content.
	pre := "# install-time config\nAPI_KEY=secret\n"
	if err := os.WriteFile(envPath, []byte(pre+"VLLM_MODEL=old-model\nOTHER=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnvVar(envPath, "VLLM_MODEL", "new/model"); err != nil {
		t.Fatalf("UpsertEnvVar update: %v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := pre + "VLLM_MODEL=new/model\nOTHER=1\n"
	if got != want {
		t.Errorf("updated env file = %q, want %q", got, want)
	}

	// Duplicate definitions collapse to one (first occurrence position).
	if err := os.WriteFile(envPath, []byte("VLLM_MODEL=a\nX=1\nVLLM_MODEL=b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnvVar(envPath, "VLLM_MODEL", "c"); err != nil {
		t.Fatalf("UpsertEnvVar dedupe: %v", err)
	}
	data, _ = os.ReadFile(envPath)
	if string(data) != "VLLM_MODEL=c\nX=1\n" {
		t.Errorf("deduped env file = %q, want VLLM_MODEL=c\\nX=1\\n", data)
	}
}

func TestReadEnvVar(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "vllm.env")

	// Missing file reads as absent.
	if _, ok := ReadEnvVar(envPath, "VLLM_MODEL"); ok {
		t.Error("ReadEnvVar on missing file reported present")
	}

	if err := os.WriteFile(envPath, []byte("# VLLM_MODEL=commented\nVLLM_MODEL=real\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if v, ok := ReadEnvVar(envPath, "VLLM_MODEL"); !ok || v != "real" {
		t.Errorf("ReadEnvVar = %q, %v; want \"real\", true (comments must be skipped)", v, ok)
	}
	if _, ok := ReadEnvVar(envPath, "ABSENT"); ok {
		t.Error("ReadEnvVar reported an absent key as present")
	}
}
