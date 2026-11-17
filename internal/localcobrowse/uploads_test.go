package localcobrowse

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func uploadFixture(t *testing.T) (string, *UploadSettings) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "local")
	if err := WriteUploadSetting(dir, "on"); err != nil {
		t.Fatal(err)
	}
	return dir, NewUploadSettings(dir, "")
}

func TestUploadsEnvironmentAndFileBothDeny(t *testing.T) {
	for _, value := range []string{"", "0", "false", "no", "off", " OFF ", "FaLsE", "on", "true", "1", "invalid-PRIVATE"} {
		t.Run(value, func(t *testing.T) {
			dir, _ := uploadFixture(t)
			s := NewUploadSettings(dir, value)
			v := strings.ToLower(strings.TrimSpace(value))
			off := v == "0" || v == "false" || v == "no" || v == "off"
			st := s.Status()
			if st.Enabled == off {
				t.Fatalf("environment %q: %+v", value, st)
			}
			if (st.Warning != "") != (v != "" && !off) {
				t.Fatalf("warning: %+v", st)
			}
			if strings.Contains(st.Warning, "PRIVATE") {
				t.Fatal("environment contents disclosed")
			}
			if err := WriteUploadSetting(dir, "off"); err != nil {
				t.Fatal(err)
			}
			if s.Status().Enabled {
				t.Fatal("environment cannot override file off")
			}
		})
	}
}

func TestUploadsMissingDefaultsOnWithoutEffects(t *testing.T) {
	if NewUploadSettings("", "").Status().Enabled {
		t.Fatal("missing local directory accepted")
	}
	if WriteUploadSetting("", "off") == nil {
		t.Fatal("wrote relative to cwd")
	}
	dir := filepath.Join(t.TempDir(), "absent")
	if !NewUploadSettings(dir, "").Status().Enabled {
		t.Fatal("missing means on")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created state")
	}
}

func TestUploadsInvalidSettingsFailClosed(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "{}", "{", `{"uploads":true}`, `{"uploads":"ON"}`, `{"uploads":"on","unknown":1}`, `{"uploads":"off","uploads":"on"}`, `{"uploads":"on"} {}`, `{"uploads":"on"}` + strings.Repeat(" ", maxUploadSettingsBytes+1)} {
		t.Run(body[:min(len(body), 50)], func(t *testing.T) {
			dir, s := uploadFixture(t)
			if !s.Status().Enabled {
				t.Fatal("fixture")
			}
			if err := os.WriteFile(filepath.Join(dir, UploadSettingsFile), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			st := s.Status()
			if st.Enabled || !strings.Contains(st.Reason, "unreadable, invalid") || strings.Contains(st.Reason, dir) {
				t.Fatalf("invalid: %+v", st)
			}
		})
	}
}

func TestUploadsLiveCacheAndReplacementIdentity(t *testing.T) {
	dir, s := uploadFixture(t)
	path := filepath.Join(dir, UploadSettingsFile)
	if !s.Status().Enabled || !s.Status().Enabled {
		t.Fatal("on cache")
	}
	before, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte("{\"uploads\":\"on\"}\nX"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if s.Status().Enabled {
		t.Fatal("size change with old mtime reused stale on")
	}
	if err := WriteUploadSetting(dir, "off"); err != nil {
		t.Fatal(err)
	}
	if s.Status().Enabled || s.Status().Enabled {
		t.Fatal("off cache")
	}
	if err := WriteUploadSetting(dir, "on"); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Enabled {
		t.Fatal("live on")
	}
	// Same inode, same size but a changed mtime must invalidate the cache.
	st, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte("{\"uploads\":\"no\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime().Add(time.Second), st.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if s.Status().Enabled {
		t.Fatal("mtime change ignored")
	}
	if err := WriteUploadSetting(dir, "on"); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Enabled {
		t.Fatal("restore")
	}
	st, _ = os.Stat(path)
	if err := WriteUploadSetting(dir, "on"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"uploads\":\"no\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if s.Status().Enabled {
		t.Fatal("new inode with old size and mtime reused stale on")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Enabled {
		t.Fatal("deletion did not clear cache")
	}
}

func TestUploadsUnsafeAndUnreadableSettings(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir, s := uploadFixture(t)
		path := filepath.Join(dir, UploadSettingsFile)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if s.Status().Enabled {
			t.Fatal("directory accepted")
		}
	})
	if runtime.GOOS == "windows" {
		return
	} // DACL checks below are cross-platform.
	for _, mode := range []os.FileMode{0, 0o644, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			dir, s := uploadFixture(t)
			if !s.Status().Enabled {
				t.Fatal("fixture")
			}
			if err := os.Chmod(filepath.Join(dir, UploadSettingsFile), mode); err != nil {
				t.Fatal(err)
			}
			if s.Status().Enabled {
				t.Fatal("permission downgrade reused cached on")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir, s := uploadFixture(t)
		path := filepath.Join(dir, UploadSettingsFile)
		if err := os.Rename(path, path+".other"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".other", path); err != nil {
			t.Fatal(err)
		}
		if s.Status().Enabled {
			t.Fatal("symlink accepted")
		}
	})
	t.Run("parent", func(t *testing.T) {
		dir, s := uploadFixture(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if s.Status().Enabled {
			t.Fatal("nonprivate parent accepted")
		}
		if err := WriteUploadSetting(dir, "on"); err == nil {
			t.Fatal("nonprivate parent repaired silently")
		}
	})
}

func TestUploadsWritePrivateReplaceAndSharedGate(t *testing.T) {
	dir, settings := uploadFixture(t)
	path := filepath.Join(dir, UploadSettingsFile)
	if err := platform.ValidatePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := platform.ValidatePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatal("file not 0600")
		}
		st, _ = os.Stat(dir)
		if st.Mode().Perm() != 0o700 {
			t.Fatal("dir not 0700")
		}
	}
	s := New(Config{Uploads: settings.Status})
	if err := s.CheckUploads(); err != nil || !s.UploadsAllowed() {
		t.Fatal("on refused")
	}
	if err := WriteUploadSetting(dir, "off"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckUploads(); !errors.Is(err, ErrUploadsDisabled) || s.UploadsAllowed() {
		t.Fatal("off bypassed shared gate")
	}
	if err := WriteUploadSetting(dir, "bad"); err == nil {
		t.Fatal("bad value written")
	}
	if settings.Status().Enabled {
		t.Fatal("invalid write changed off")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != UploadSettingsFile {
		t.Fatalf("temp leak: %v %v", entries, err)
	}
}

func TestUploadsConcurrentGateReaders(t *testing.T) {
	dir, settings := uploadFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = settings.Status()
			}
		}()
	}
	for i := 0; i < 10; i++ {
		if err := WriteUploadSetting(dir, "off"); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if settings.Status().Enabled {
		t.Fatal("final off lost")
	}
}

func TestUploadsSettingsBoundAndFailedReplaceCleanup(t *testing.T) {
	if maxUploadSettingsBytes != 4096 {
		t.Fatal("settings read bound changed")
	}
	dir, _ := uploadFixture(t)
	path := filepath.Join(dir, UploadSettingsFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteUploadSetting(dir, "off"); err == nil {
		t.Fatal("replaced non-file destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("failed replace leaked temp: %v %v", entries, err)
	}
}
