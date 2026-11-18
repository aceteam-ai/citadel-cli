package localcobrowse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const UploadSettingsFile = "settings.json"
const UploadSettingsDir = "local-settings"
const maxUploadSettingsBytes = 4096

var ErrUploadsDisabled = errors.New("uploads_off: uploads are turned off on this computer; check citadel local config uploads status")

// UploadStatus contains fixed, path-free explanations suitable for the viewer.
type UploadStatus struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
	Warning string `json:"warning,omitempty"`
}

// UploadSettings captures the environment once, but checks the file on every
// use. A stat failure never reuses an earlier successful reading.
type UploadSettings struct {
	root    string
	dir     string
	envOff  bool
	warning string
	mu      sync.Mutex
	info    os.FileInfo
	fileOn  bool
	// Per-instance filesystem seams keep identity/read races deterministic in
	// tests without mutating process-wide hooks or touching real user state.
	open func(string) (*os.File, error)
	read func(*os.File) ([]byte, error)
	stat func(string) (os.FileInfo, error)
}

func NewUploadSettings(dir, environment string) *UploadSettings {
	s := &UploadSettings{root: dir, open: platform.OpenPrivateFile, stat: os.Lstat,
		read: func(f *os.File) ([]byte, error) { return io.ReadAll(io.LimitReader(f, maxUploadSettingsBytes+1)) }}
	if dir != "" {
		s.dir = filepath.Join(dir, UploadSettingsDir)
	}
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "", "1", "true", "yes", "on":
	case "0", "false", "no", "off":
		s.envOff = true
	default:
		s.warning = "CITADEL_LOCAL_UPLOADS has an unrecognized value; ignored (only 0, false, no or off disable uploads)"
	}
	return s
}

func (s *UploadSettings) Status() UploadStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	on, reason := s.readFile()
	if s.envOff {
		on, reason = false, "Uploads are turned off by CITADEL_LOCAL_UPLOADS"
	}
	return UploadStatus{Enabled: on, Reason: reason, Warning: s.warning}
}

// Windows may report a non-directory ancestor as PATH_NOT_FOUND. Walk to the
// first existing ancestor before treating a directory as genuinely absent.
func (s *UploadSettings) absentDirectory(path string) bool {
	for {
		st, err := s.stat(path)
		if err == nil {
			return st.IsDir()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
}

func (s *UploadSettings) readFile() (bool, string) {
	const bad = "Uploads are off because settings.json is unreadable, invalid or not owner-only; use citadel local config uploads on|off to replace it"
	if s.dir == "" {
		return false, bad
	}
	// A missing setting is on only inside a trusted existing container, or
	// when no local state directory exists at all. A removed off file must not
	// bypass a permission downgrade of either existing directory.
	if _, err := s.stat(s.root); err == nil {
		if platform.ValidateSettingsParent(s.root) != nil {
			s.info = nil
			return false, bad
		}
	} else if !errors.Is(err, os.ErrNotExist) || !s.absentDirectory(s.root) {
		s.info = nil
		return false, bad
	}
	if _, err := s.stat(s.dir); err == nil {
		if platform.ValidatePrivateDir(s.dir) != nil {
			s.info = nil
			return false, bad
		}
	} else if !errors.Is(err, os.ErrNotExist) || !s.absentDirectory(s.dir) {
		s.info = nil
		return false, bad
	}
	path := filepath.Join(s.dir, UploadSettingsFile)
	st, err := s.stat(path)
	if errors.Is(err, os.ErrNotExist) {
		s.info = nil
		if !s.absentDirectory(s.dir) {
			return false, bad
		}
		return true, "Uploads are on (no setting file)"
	}
	if err != nil || platform.ValidatePrivateDir(s.dir) != nil || platform.ValidatePrivateFile(path) != nil || st.Size() > maxUploadSettingsBytes || st.Mode().Perm()&0o400 == 0 {
		s.info = nil
		return false, bad
	}
	// os.Lstat on Windows leaves the file ID lazy. Force its acquisition
	// before the open boundary, otherwise SameFile can resolve the old info's
	// path only after replacement and compare the replacement to itself.
	if !os.SameFile(st, st) {
		s.info = nil
		return false, bad
	}
	if s.info != nil && os.SameFile(s.info, st) && s.info.Size() == st.Size() && s.info.ModTime() == st.ModTime() {
		if s.fileOn {
			return true, "Uploads are on in settings.json"
		}
		return false, "Uploads are turned off in settings.json"
	}
	f, err := s.open(path)
	if err != nil {
		s.info = nil
		return false, bad
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() {
		s.info = nil
		return false, bad
	}
	b, err := s.read(f)
	after, statErr := f.Stat()
	if err != nil || statErr != nil || len(b) > maxUploadSettingsBytes || after.Size() != st.Size() || after.ModTime() != st.ModTime() {
		s.info = nil
		return false, bad
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	open, e1 := dec.Token()
	key, e2 := dec.Token()
	var value string
	e3 := dec.Decode(&value)
	close, e4 := dec.Token()
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || open != json.Delim('{') || key != "uploads" || close != json.Delim('}') || dec.Decode(new(any)) != io.EOF || (value != "on" && value != "off") {
		s.info = nil
		return false, bad
	}
	s.info, s.fileOn = st, value == "on"
	if s.fileOn {
		return true, "Uploads are on in settings.json"
	}
	return false, "Uploads are turned off in settings.json"
}

// WriteUploadSetting only replaces local mode's settings file. It never reads
// or edits a harness configuration, and never follows an existing file symlink.
func WriteUploadSetting(dir, value string) error {
	if dir == "" {
		return errors.New("local settings directory is unavailable")
	}
	if value != "on" && value != "off" {
		return errors.New("uploads must be on or off")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("cannot create local state directory")
	}
	if err := platform.ValidateSettingsParent(dir); err != nil {
		return errors.New("local state directory has an untrusted writer")
	}
	dir = filepath.Join(dir, UploadSettingsDir)
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return errors.New("cannot create private local settings directory")
	}
	if err := platform.ValidatePrivateDir(dir); err != nil {
		return errors.New("local settings directory is not owner-only")
	}
	f, err := os.CreateTemp(dir, ".settings-*")
	if err != nil {
		return errors.New("cannot create local settings file")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := platform.SecurePrivateFile(f.Name()); err != nil {
		return errors.New("cannot protect local settings file")
	}
	if _, err := f.WriteString("{\"uploads\":\"" + value + "\"}\n"); err != nil {
		return errors.New("cannot write local settings file")
	}
	if err := f.Sync(); err != nil {
		return errors.New("cannot sync local settings file")
	}
	if err := f.Close(); err != nil {
		return errors.New("cannot close local settings file")
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, UploadSettingsFile)); err != nil {
		return errors.New("cannot replace local settings file")
	}
	return nil
}

// UploadStatus is advisory; every operation must recheck the shared gate just
// before its effect. Later staging/attach slices call CheckUploads and the
// platform's dispatch fence calls UploadsAllowed. No upload entry ships in S2.
func (s *Service) UploadStatus() UploadStatus {
	if s.cfg.Uploads != nil {
		return s.cfg.Uploads()
	}
	return UploadStatus{Enabled: false, Reason: "Uploads are off because this service has no upload policy"}
}
func (s *Service) uploadsAllowed() bool { return s.UploadStatus().Enabled }
func (s *Service) UploadsAllowed() bool { return s.uploadsAllowed() }
func (s *Service) CheckUploads() error {
	if !s.uploadsAllowed() {
		return ErrUploadsDisabled
	}
	return nil
}
