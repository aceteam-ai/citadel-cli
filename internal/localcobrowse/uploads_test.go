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
	for _, value := range []string{"", "0", "false", "no", "off", " OFF ", "FaLsE", "on", "true", "1", " yes ", "invalid-PRIVATE"} {
		t.Run(value, func(t *testing.T) {
			dir, _ := uploadFixture(t)
			s := NewUploadSettings(dir, value)
			v := strings.ToLower(strings.TrimSpace(value))
			off := v == "0" || v == "false" || v == "no" || v == "off"
			st := s.Status()
			if st.Enabled == off {
				t.Fatalf("environment %q: %+v", value, st)
			}
			if (st.Warning != "") != (v == "invalid-private") {
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

func TestUploadsMissingBelowSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires a Windows privilege")
	}
	base := t.TempDir()
	link := filepath.Join(base, "home-link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(link, "absent", "local")
	if !NewUploadSettings(dir, "").Status().Enabled {
		t.Fatal("directory ancestor symlink refused missing-file default")
	}
	if _, err := os.Lstat(filepath.Join(base, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created state through ancestor symlink")
	}
	// Only ancestors above the state directory may resolve through a symlink.
	if NewUploadSettings(link, "").Status().Enabled {
		t.Fatal("symlink at the state directory accepted")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{file, filepath.Join(base, "missing")} {
		badLink := filepath.Join(t.TempDir(), "bad-link")
		if err := os.Symlink(target, badLink); err != nil {
			t.Fatal(err)
		}
		if NewUploadSettings(filepath.Join(badLink, "local"), "").Status().Enabled {
			t.Fatal("non-directory or dangling ancestor symlink accepted")
		}
	}
}

func TestUploadsCacheRechecksOpenPermission(t *testing.T) {
	_, s := uploadFixture(t)
	reads := 0
	var opened []*os.File
	originalOpen := s.open
	s.open = func(path string) (*os.File, error) {
		f, err := originalOpen(path)
		if err == nil {
			opened = append(opened, f)
		}
		return f, err
	}
	read := s.read
	s.read = func(f *os.File) ([]byte, error) {
		reads++
		return read(f)
	}
	if !s.Status().Enabled || !s.Status().Enabled || reads != 1 {
		t.Fatalf("parsed settings cache not used: reads=%d", reads)
	}
	if len(opened) != 2 {
		t.Fatal("cache hit did not open with current read permission")
	}
	for _, f := range opened {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("gate leaked an opened descriptor")
		}
	}
	trackedOpen := s.open
	s.open = func(string) (*os.File, error) { return nil, os.ErrPermission }
	if s.Status().Enabled {
		t.Fatal("cached on bypassed a newly unreadable settings file")
	}
	s.open = trackedOpen
	if !s.Status().Enabled || reads != 2 {
		t.Fatal("permission failure did not discard the cached reading")
	}
}

func TestUploadsCacheRejectsMetadataChangingDuringOpen(t *testing.T) {
	for _, field := range []string{"size", "mtime"} {
		t.Run(field, func(t *testing.T) {
			_, s := uploadFixture(t)
			if !s.Status().Enabled {
				t.Fatal("fixture")
			}
			open := s.open
			s.open = func(path string) (*os.File, error) {
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				changed := before.ModTime()
				if field == "size" {
					if err := os.WriteFile(path, []byte("{\"uploads\":\"on\"}\n "), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					changed = changed.Add(time.Second)
				}
				if err := os.Chtimes(path, changed, changed); err != nil {
					t.Fatal(err)
				}
				return open(path)
			}
			if s.Status().Enabled {
				t.Fatal("cached on bypassed " + field + " changing during open")
			}
		})
	}
}

func TestUploadsInvalidSettingsFailClosed(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "{}", "{", `{"uploads":true}`, `{"uploads":"ON"}`, `{"other":"on"}`, `{"Uploads":"on"}`, `{"uploads":"on","unknown":1}`, `{"uploads":"off","uploads":"on"}`, `{"uploads":"on"} {}`, `{"uploads":"on"}` + strings.Repeat(" ", maxUploadSettingsBytes+1)} {
		t.Run(body[:min(len(body), 50)], func(t *testing.T) {
			dir, s := uploadFixture(t)
			if !s.Status().Enabled {
				t.Fatal("fixture")
			}
			if err := os.WriteFile(filepath.Join(dir, UploadSettingsDir, UploadSettingsFile), []byte(body), 0o600); err != nil {
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
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
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
		path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
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
			if err := os.Chmod(filepath.Join(dir, UploadSettingsDir, UploadSettingsFile), mode); err != nil {
				t.Fatal(err)
			}
			if s.Status().Enabled {
				t.Fatal("permission downgrade reused cached on")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir, s := uploadFixture(t)
		path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
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
		if err := os.Chmod(filepath.Join(dir, UploadSettingsDir), 0o755); err != nil {
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
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
	if err := platform.ValidatePrivateDir(filepath.Join(dir, UploadSettingsDir)); err != nil {
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
		st, _ = os.Stat(filepath.Join(dir, UploadSettingsDir))
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
	entries, err := os.ReadDir(filepath.Join(dir, UploadSettingsDir))
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

func TestUploadsUnwiredServiceDenies(t *testing.T) {
	s := New(Config{})
	if s.UploadsAllowed() || !errors.Is(s.CheckUploads(), ErrUploadsDisabled) {
		t.Fatal("unwired gate enabled")
	}
}

func TestUploadsStatErrorsDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{path, filepath.Join(path, "child")} {
		if NewUploadSettings(dir, "").Status().Enabled {
			t.Fatal("ENOTDIR or non-directory accepted")
		}
	}
	// Model Windows ERROR_PATH_NOT_FOUND for a non-directory ancestor on
	// every OS, so CI cannot regress this into the missing-file default.
	simulated := NewUploadSettings(filepath.Join(path, "child"), "")
	simulated.stat = func(candidate string) (os.FileInfo, error) {
		if strings.HasPrefix(candidate, path+string(os.PathSeparator)) {
			return nil, os.ErrNotExist
		}
		return os.Lstat(candidate)
	}
	if simulated.Status().Enabled {
		t.Fatal("non-directory ancestor disguised as not found enabled uploads")
	}
	dir, s := uploadFixture(t)
	if !s.Status().Enabled {
		t.Fatal("fixture")
	}
	s.stat = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(dir, UploadSettingsDir, UploadSettingsFile) {
			return nil, os.ErrPermission
		}
		return os.Lstat(path)
	}
	if s.Status().Enabled {
		t.Fatal("file stat error reused cache or treated as missing")
	}
}

func TestUploadsOpenedIdentityAndPostReadChangesDeny(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		dir, s := uploadFixture(t)
		s.open = func(path string) (*os.File, error) {
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteUploadSetting(dir, "on"); err != nil {
				t.Fatal(err)
			}
			// Keep size and mtime equal so only the handle identity fence can
			// detect this replacement, not the independent post-read checks.
			if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			return platform.OpenPrivateFile(path)
		}
		if s.Status().Enabled {
			t.Fatal("replacement between lstat and open accepted")
		}
	})
	for _, field := range []string{"size", "mtime"} {
		t.Run(field, func(t *testing.T) {
			dir, s := uploadFixture(t)
			read := s.read
			s.read = func(f *os.File) ([]byte, error) {
				b, err := read(f)
				path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
				if field == "size" {
					before, statErr := f.Stat()
					if statErr != nil {
						t.Fatal(statErr)
					}
					if err := os.WriteFile(path, append(b, ' '), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
						t.Fatal(err)
					}
				} else {
					st, err := f.Stat()
					if err != nil {
						t.Fatal(err)
					}
					changed := st.ModTime().Add(time.Second)
					if err := os.Chtimes(path, changed, changed); err != nil {
						t.Fatal(err)
					}
				}
				return b, err
			}
			if s.Status().Enabled {
				t.Fatal("post-read " + field + " change accepted")
			}
		})
	}
	t.Run("post-read stat error", func(t *testing.T) {
		_, s := uploadFixture(t)
		read := s.read
		s.read = func(f *os.File) ([]byte, error) { b, err := read(f); _ = f.Close(); return b, err }
		if s.Status().Enabled {
			t.Fatal("post-read stat error accepted")
		}
	})
}

func TestUploadsMissingFileDoesNotBypassUntrustedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACL regression runs in platform/command tests")
	}
	for _, which := range []string{"state", "settings"} {
		t.Run(which, func(t *testing.T) {
			dir, s := uploadFixture(t)
			if err := WriteUploadSetting(dir, "off"); err != nil {
				t.Fatal(err)
			}
			unsafe := dir
			mode := os.FileMode(0o777)
			if which == "settings" {
				unsafe = filepath.Join(dir, UploadSettingsDir)
				mode = 0o755
			}
			if err := os.Chmod(unsafe, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)); err != nil {
				t.Fatal(err)
			}
			if s.Status().Enabled || NewUploadSettings(dir, "").Status().Enabled {
				t.Fatal("missing off file bypassed directory trust")
			}
		})
	}
}

func TestUploadsUntrustedStateContainerRefusesWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode regression; Windows parent DACL tests run separately")
	}
	dir, s := uploadFixture(t)
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := WriteUploadSetting(dir, "off"); err == nil {
		t.Fatal("writer accepted an untrusted state container")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("refused write changed settings")
	}
	if s.Status().Enabled {
		t.Fatal("untrusted state container allowed cached on")
	}
}

func TestUploadsSettingsBoundAndFailedReplaceCleanup(t *testing.T) {
	if maxUploadSettingsBytes != 4096 {
		t.Fatal("settings read bound changed")
	}
	dir, _ := uploadFixture(t)
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteUploadSetting(dir, "off"); err == nil {
		t.Fatal("replaced non-file destination")
	}
	entries, err := os.ReadDir(filepath.Join(dir, UploadSettingsDir))
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("failed replace leaked temp: %v %v", entries, err)
	}
}
