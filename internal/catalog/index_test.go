package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleIndex = `
version: 1
modules:
  - name: whisper
    source: acme/whisper
    description: Speech-to-text inference service
    tags: [audio, asr, gpu]
  - name: bge-embed
    source: other/bge
    description: Text embedding engine
    tags: [embedding, nlp]
  - name: plainsource
    source: https://git.example.com/x/y.git
    description: A module with no tags
`

func TestParseModuleIndex(t *testing.T) {
	idx, err := ParseModuleIndex([]byte(sampleIndex))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if idx.Version != 1 {
		t.Errorf("version = %d, want 1", idx.Version)
	}
	if len(idx.Modules) != 3 {
		t.Fatalf("got %d modules, want 3", len(idx.Modules))
	}
	if idx.Modules[0].Name != "whisper" || idx.Modules[0].Source != "acme/whisper" {
		t.Errorf("unexpected first entry: %+v", idx.Modules[0])
	}
}

func TestParseModuleIndex_DefaultsVersion(t *testing.T) {
	idx, err := ParseModuleIndex([]byte("modules: []"))
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != 1 {
		t.Errorf("expected version defaulted to 1, got %d", idx.Version)
	}
}

func TestSearchModuleIndex(t *testing.T) {
	idx, _ := ParseModuleIndex([]byte(sampleIndex))
	tests := []struct {
		query string
		want  []string // expected names, in order
	}{
		{"whisper", []string{"whisper"}},
		{"WHISPER", []string{"whisper"}},                      // case-insensitive name
		{"embedding", []string{"bge-embed"}},                  // tag
		{"engine", []string{"bge-embed"}},                     // description
		{"acme", []string{"whisper"}},                         // source substring
		{"gpu", []string{"whisper"}},                          // tag
		{"", []string{"whisper", "bge-embed", "plainsource"}}, // empty = all
		{"nomatch", nil},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := SearchModuleIndex(idx, tt.query)
			if len(got) != len(tt.want) {
				t.Fatalf("query %q: got %d results, want %d (%v)", tt.query, len(got), len(tt.want), got)
			}
			for i, name := range tt.want {
				if got[i].Name != name {
					t.Errorf("query %q result[%d] = %q, want %q", tt.query, i, got[i].Name, name)
				}
			}
		})
	}
}

func TestSearchModuleIndex_NilSafe(t *testing.T) {
	if got := SearchModuleIndex(nil, "x"); got != nil {
		t.Errorf("expected nil for nil index, got %v", got)
	}
}

func TestLoadModuleIndex_MissingIsFailSoft(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No catalog cloned, no index file -> empty index, nil error.
	idx, err := LoadModuleIndex()
	if err != nil {
		t.Fatalf("expected fail-soft nil error, got %v", err)
	}
	if len(idx.Modules) != 0 {
		t.Errorf("expected empty index, got %d modules", len(idx.Modules))
	}
}

func TestLoadModuleIndex_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "myindex.yaml")
	if err := os.WriteFile(path, []byte(sampleIndex), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ModuleIndexEnv, path)
	idx, err := LoadModuleIndex()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(idx.Modules) != 3 {
		t.Errorf("expected 3 modules from env-override index, got %d", len(idx.Modules))
	}
}

func TestLoadModuleIndex_MalformedErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("modules: [this is : not valid"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ModuleIndexEnv, path)
	if _, err := LoadModuleIndex(); err == nil {
		t.Error("expected error for malformed index")
	}
}
