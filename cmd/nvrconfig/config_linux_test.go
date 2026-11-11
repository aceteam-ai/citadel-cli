//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != want {
		t.Errorf("mode = %04o, want %04o", st.Mode().Perm(), want)
	}
}

func setAssignment(t *testing.T) {
	t.Helper()
	for _, pair := range [][2]string{
		{"NVR_DETECTOR", "cpu"},
		{"NVR_RETENTION_DAYS", "12"},
		{"NVR_STORAGE_MODE", "local"},
		{"NVR_STORAGE_TARGET", ""},
		{"NVR_CAMERAS", "front-door"},
		{"NVR_SEMANTIC_SEARCH", "false"},
		{"NVR_TRACK_OBJECTS", "person"},
		{"NVR_MQTT", "true"},
		{"NVR_MQTT_HOST", "mosquitto"},
		{"NVR_MQTT_PORT", "1883"},
		{"NVR_MQTT_CREDENTIALS_FROM_ENV", "true"},
		{"NVR_MQTT_USER", "private-broker-user"},
		{"NVR_MQTT_PASSWORD", "private-broker-password-$:'\"{}"},
		{"NVR_MQTT_TOPIC_PREFIX", "frigate"},
	} {
		t.Setenv(pair[0], pair[1])
	}
}

func TestPrivateConfigReplacesLegacyInodeAndNeverWritesCredentials(t *testing.T) {
	setAssignment(t)
	path := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, configName)
	if err := os.WriteFile(file, []byte("legacy plaintext"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := openPrivateConfigDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	assertMode(t, path, 0o700)
	assertMode(t, file, 0o600)
	if err := runWithConfigDir(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, file, 0o600)
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("config must be atomically replaced, not truncated in place")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"NVR_MQTT_USER", "NVR_MQTT_PASSWORD"} {
		if strings.Contains(string(data), os.Getenv(key)) {
			t.Errorf("generated config contains plaintext %s", key)
		}
	}
	var cfg struct {
		MQTT struct {
			User     string `yaml:"user"`
			Password string `yaml:"password"`
		} `yaml:"mqtt"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.User != "{FRIGATE_MQTT_USER}" || cfg.MQTT.Password != "{FRIGATE_MQTT_PASSWORD}" {
		t.Error("MQTT credentials must be fixed environment placeholders")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != configName {
		t.Errorf("unexpected files after replacement: %v, %v", entries, err)
	}
}

func TestPrivateConfigHardensBothNamesBeforeFailedRegeneration(t *testing.T) {
	setAssignment(t)
	t.Setenv("NVR_RETENTION_DAYS", "invalid")
	path := t.TempDir()
	for _, name := range []string{configName, "config.yaml"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte("old secret"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := openPrivateConfigDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := runWithConfigDir(dir); err == nil {
		t.Fatal("invalid assignment must fail")
	}
	for _, name := range []string{configName, "config.yaml"} {
		file := filepath.Join(path, name)
		assertMode(t, file, 0o600)
		data, err := os.ReadFile(file)
		if err != nil || string(data) != "old secret" {
			t.Errorf("failed generation changed existing config: %v", err)
		}
	}
}

func TestPrivateConfigRejectsUnsafeExistingFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "outside")
			if err := os.WriteFile(outside, []byte("must not change"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, "config")
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(path, configName)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, file)
			case "hardlink":
				err = os.Link(outside, file)
			case "directory":
				err = os.Mkdir(file, 0o755)
			case "fifo":
				err = unix.Mkfifo(file, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if dir, err := openPrivateConfigDir(path); err == nil {
				dir.Close()
				t.Fatal("unsafe existing config accepted")
			}
			assertMode(t, outside, 0o644)
		})
	}
}

func TestPrivateConfigRejectsSymlinkDirectory(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "config")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if dir, err := openPrivateConfigDir(link); err == nil {
		dir.Close()
		t.Fatal("symlink directory accepted")
	}
	assertMode(t, outside, 0o755)
}

func TestPrivateConfigCreatesDirectoryAndCleansFailedRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-config")
	dir, err := openPrivateConfigDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	assertMode(t, path, 0o700)
	// A late target-directory collision makes rename fail after the temp write.
	if err := os.Mkdir(filepath.Join(path, configName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateConfig(dir, []byte("new config")); err == nil {
		t.Fatal("rename over a directory must fail")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != configName {
		t.Errorf("temporary file leaked after failure: %v, %v", entries, err)
	}
}

func TestComposeSuppliesFrigateCredentialsAndPinsCachedInitImage(t *testing.T) {
	data, err := os.ReadFile("../../services/nvr-service/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Image       string   `yaml:"image"`
			User        string   `yaml:"user"`
			PullPolicy  string   `yaml:"pull_policy"`
			Environment []string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}
	// The former always-refresh assertion protected a floating image. #1396
	// replaces it with this verified immutable security-repair image; do not
	// allow the cache policy to conceal an older/unpinned init binary.
	const source = "2241cf4d5a171a0b11b733b84ec06035f185ab85"
	const digest = "dcbe4f783eb14c7979d93db68ae9239eada5fec09f71d5966aa733b20b2eebba"
	const reviewedImage = "ghcr.io/aceteam-ai/nvr-config:" + source + "@sha256:" + digest
	if compose.Services["nvr-config"].Image != reviewedImage {
		t.Error("compose must pin the reviewed security-repair init image")
	}
	if compose.Services["nvr-config"].PullPolicy != "missing" {
		t.Error("the exact pinned init image must start from cache without mandatory refresh")
	}
	for _, service := range []string{"nvr-config", "frigate"} {
		if compose.Services[service].User != "" {
			t.Errorf("%s unexpectedly overrides the stock root identity", service)
		}
	}
	initOptIn := false
	for _, item := range compose.Services["nvr-config"].Environment {
		if item == "NVR_MQTT_CREDENTIALS_FROM_ENV=true" {
			initOptIn = true
		}
	}
	if !initOptIn {
		t.Error("new compose must opt in alongside matching Frigate environment")
	}
	env := make(map[string]string)
	for _, item := range compose.Services["frigate"].Environment {
		key, value, _ := strings.Cut(item, "=")
		env[key] = value
	}
	if env["FRIGATE_MQTT_USER"] != "${NVR_MQTT_USER:-frigate}" {
		t.Error("Frigate username must use the broker assignment source")
	}
	if env["FRIGATE_MQTT_PASSWORD"] != "${NVR_MQTT_PASSWORD:?nvr requires NVR_MQTT_PASSWORD (mosquitto allows anonymous access by default)}" {
		t.Error("Frigate password must use the required broker assignment source")
	}
}

func TestPrivateConfigLegacyComposeWritesPrivateLiterals(t *testing.T) {
	for _, optIn := range []string{"", "false"} {
		t.Run("opt-in="+optIn, func(t *testing.T) {
			setAssignment(t)
			t.Setenv("NVR_MQTT_CREDENTIALS_FROM_ENV", optIn)
			path := filepath.Join(t.TempDir(), "config")
			dir, err := openPrivateConfigDir(path)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			if err := runWithConfigDir(dir); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(path, configName)
			assertMode(t, path, 0o700)
			assertMode(t, file, 0o600)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				MQTT struct {
					User     string `yaml:"user"`
					Password string `yaml:"password"`
				} `yaml:"mqtt"`
			}
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.MQTT.User != os.Getenv("NVR_MQTT_USER") || cfg.MQTT.Password != os.Getenv("NVR_MQTT_PASSWORD") {
				t.Error("legacy compose credentials changed")
			}
			if strings.Contains(string(data), "{FRIGATE_MQTT_") {
				t.Error("legacy compose received unresolvable placeholders")
			}
		})
	}
}

func TestPrivateConfigRejectsInvalidCredentialOptIn(t *testing.T) {
	setAssignment(t)
	t.Setenv("NVR_MQTT_CREDENTIALS_FROM_ENV", "not-a-bool")
	dir, err := openPrivateConfigDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := runWithConfigDir(dir); err == nil {
		t.Fatal("invalid credential opt-in must fail, not silently choose literals")
	}
}
