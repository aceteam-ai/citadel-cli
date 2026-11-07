package services

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func keepAlivePointer(value string) *string { return &value }

func TestOllamaKeepAlivePolicy(t *testing.T) {
	base := []string{"UNRELATED=keep", EnvOllamaKeepAlive + "=73h", EnvOllamaKeepAlive + "=91h", "OLLAMA_KEEP_ALIVE_EXTRA=untouched"}
	for _, tc := range []struct {
		name   string
		policy *string
		want   string
		bad    bool
	}{
		{"unset", nil, "", false},
		{"duration", keepAlivePointer("60m"), "60m", false},
		{"composite", keepAlivePointer("1h30m"), "1h30m", false},
		{"fraction", keepAlivePointer("1.5h"), "1.5h", false},
		{"infinite", keepAlivePointer("-1"), "-1", false},
		{"zero", keepAlivePointer("0"), "0", false},
		{"zero-duration", keepAlivePointer("0s"), "0s", false},
		{"trimmed", keepAlivePointer(" 10m "), "10m", false},
		{"empty", keepAlivePointer(""), "", true},
		{"negative-duration", keepAlivePointer("-1m"), "", true},
		{"negative-integer", keepAlivePointer("-2"), "", true},
		{"unitless", keepAlivePointer("600"), "", true},
		{"invalid", keepAlivePointer("forever"), "", true},
		{"overflow", keepAlivePointer("999999999999999999999h"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]string{}, base...)
			got, err := OllamaKeepAliveComposeEnv(base, "ollama", tc.policy)
			if (err != nil) != tc.bad {
				t.Fatalf("error %v, bad=%v", err, tc.bad)
			}
			want := []string{"UNRELATED=keep", "OLLAMA_KEEP_ALIVE_EXTRA=untouched", EnvOllamaKeepAlive + "=" + tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if !reflect.DeepEqual(base, before) {
				t.Fatal("input env mutated")
			}
			got, err = OllamaKeepAliveComposeEnv(base, "llamacpp", tc.policy)
			if err != nil || !reflect.DeepEqual(got, base) {
				t.Fatal("non-Ollama environment changed")
			}
		})
	}
}

func TestOllamaKeepAliveEmbeddedPassthrough(t *testing.T) {
	for _, name := range []string{"compose/ollama.yml", "compose/ollama.darwin.yml"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Environment []string `yaml:"environment"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range doc.Services["ollama"].Environment {
			if entry == EnvOllamaKeepAlive {
				count++
			} else if strings.HasPrefix(entry, EnvOllamaKeepAlive+"=") {
				t.Fatal("template replaces authoritative environment")
			}
		}
		if count != 1 {
			t.Fatalf("%s passthrough count %d", name, count)
		}
	}
}

func TestOllamaKeepAliveComposePrecedence(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker Compose config renderer unavailable")
	}
	probeDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, docker, "compose", "version")
	probe.Env = []string{"HOME=" + probeDir, "DOCKER_CONFIG=" + filepath.Join(probeDir, "docker")}
	if err := probe.Run(); err != nil {
		t.Skipf("Docker Compose config renderer unavailable: %v", err)
	}
	for _, template := range []string{"ollama.yml", "ollama.darwin.yml"} {
		t.Run(template, func(t *testing.T) {
			dir := t.TempDir()
			source, err := os.ReadFile(filepath.Join("compose", template))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, template)
			if err := os.WriteFile(path, source, 0600); err != nil {
				t.Fatal(err)
			}
			envFile := filepath.Join(dir, "fixture.env")
			if err := os.WriteFile(envFile, []byte(EnvOllamaKeepAlive+"=88h\n"), 0600); err != nil {
				t.Fatal(err)
			}
			base := []string{"HOME=" + dir, "DOCKER_CONFIG=" + filepath.Join(dir, "docker")}
			if got := renderOllamaEnvironmentValue(t, docker, dir, path, envFile, base, EnvOllamaKeepAlive); got != "88h" {
				t.Fatalf("missing-key control did not use env file: %q", got)
			}
			for _, value := range []string{"", "60m", "-1", "0"} {
				var policy *string
				if value != "" {
					policy = &value
				}
				env, err := OllamaKeepAliveComposeEnv(append(append([]string{}, base...), EnvOllamaKeepAlive+"=99h"), "ollama", policy)
				if err != nil {
					t.Fatal(err)
				}
				if got := renderOllamaEnvironmentValue(t, docker, dir, path, envFile, env, EnvOllamaKeepAlive); got != value {
					t.Fatalf("manifest %q rendered as %q", value, got)
				}
			}
		})
	}
}
