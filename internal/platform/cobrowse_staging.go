// Per-session file staging and file inputs for co-browse sessions
// (citadel-cli#1721, node side of aceteam#13831).
//
// A remotely dispatched session runs without a rendered file chooser, so a
// page's <input type=file> can never be filled by a person or by a script. Two
// pieces fix that without exposing the node's filesystem:
//
//   - StageFile writes bytes the platform supplies into a private staging dir
//     that belongs to one session. The dir sits inside the session's own
//     working dir, so every path that removes the session (Stop, a failed
//     launch, the orphan sweep, an expired record) removes it too.
//   - SetFileInput fills a file input with one staged file through CDP
//     DOM.setFileInputFiles. It runs as an ordinary scripted action (one
//     connection, the action deadline, the page policy and pin rules, refused
//     while a human drives), and it only ever hands the browser a regular file
//     that sits directly inside this session's staging dir.
//
// Neither the name nor the bytes of a staged file is ever treated as a path
// component the caller controls beyond one validated base name, and the audit
// lines carry name, size and sha256, never content.
package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v3/disk"
)

const (
	// MaxCobrowseStagedFileBytes caps one staged file. It is half of the
	// 50 MB FILE_WRITE_BYTES ceiling: the bytes arrive base64 inside one job
	// payload (about 33 MB on the wire at this size), and the platform waits
	// only seconds for a job result, so a bigger file would not make it through
	// the relay reliably anyway. 25 MiB covers a filing PDF or a scan set.
	MaxCobrowseStagedFileBytes = 25 << 20
	// MaxCobrowseStagedFiles caps how many files one session may hold, so a
	// session cannot be used as free disk (16 x 25 MiB at most).
	MaxCobrowseStagedFiles = 16

	// minFreeAfterStageBytes is the free space that must remain on the staging
	// filesystem after a write. 1 GiB leaves the browser room for its own profile,
	// cache and downloads, and keeps a full-size staged file (25 MiB) from being
	// the write that fills a small disk. If free space cannot be read the write
	// proceeds, like the other disk preflights here.
	minFreeAfterStageBytes = 1 << 30

	cobrowseStagingDirName = ".citadel-stage"
	maxStagedNameBytes     = 128
)

// StagedFile describes one staged file. Never its content.
type StagedFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Staging refusals.
var (
	ErrStagedNameInvalid = errors.New("invalid staged file name")
	ErrStagedTooLarge    = fmt.Errorf("staged file exceeds the %d byte limit", MaxCobrowseStagedFileBytes)
	ErrStagedPathRefused = errors.New("staged path refused")
)

// availableStageBytes reports free bytes at path. A variable so tests can
// simulate a full disk.
var availableStageBytes = func(path string) (uint64, error) {
	u, err := disk.Usage(path)
	if err != nil {
		return 0, err
	}
	return u.Free, nil
}

// stagingFailure logs the detail (which may name a path) and returns an error
// that names none: errors cross to the platform, and a session path is node
// internals.
func stagingFailure(what string, err error) error {
	log.Printf("[cobrowse] staging: %s: %v", what, err)
	return errors.New("staging: " + what)
}

// windowsReservedNames are device names Windows resolves regardless of
// extension. They are refused on every OS so a name staged on one host is
// valid everywhere.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
}

// ValidateStagedName accepts exactly one plain base name: no separators, no
// NUL or control characters, not empty, ".", "..", a leading dot, a Windows
// reserved device name, a trailing dot or space, or any of the characters
// Windows forbids in file names.
func ValidateStagedName(name string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrStagedNameInvalid, why) }
	switch {
	case name == "":
		return bad("empty")
	case name == "." || name == "..":
		return bad("dot name")
	case len(name) > maxStagedNameBytes:
		return bad("too long")
	case !utf8.ValidString(name):
		return bad("not valid UTF-8")
	case strings.HasPrefix(name, "."):
		return bad("leading dot")
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return bad("trailing dot or space")
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return bad("control or format character")
		}
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return bad("forbidden character")
		}
	}
	if filepath.Base(name) != name || filepath.IsAbs(name) || filepath.VolumeName(name) != "" || !filepath.IsLocal(name) {
		return bad("not a single base name")
	}
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	if windowsReservedNames[stem] {
		return bad("reserved device name")
	}
	// COM0-9 and LPT0-9, including the superscript digits Windows also treats
	// as device numbers.
	if r := []rune(stem); len(r) == 4 && (string(r[:3]) == "COM" || string(r[:3]) == "LPT") && strings.ContainsRune("0123456789¹²³", r[3]) {
		return bad("reserved device name")
	}
	return nil
}

// stagingDirPath is where this session's staged files live.
func (s *cobrowseSession) stagingDirPath() string {
	return filepath.Join(s.profile, cobrowseStagingDirName)
}

// ensureStagingDir creates the staging dir (owner only; on Windows it
// inherits the protected owner-only DACL of the session base dir) and refuses
// anything that is not a real directory only this user can reach. It creates
// the leaf only: the session dir must already exist, so a stage racing a
// teardown can never recreate a removed session dir.
func (s *cobrowseSession) ensureStagingDir() (string, error) {
	dir := s.stagingDirPath()
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", stagingFailure("create staging dir", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", stagingFailure("inspect staging dir", err)
	}
	// A symlink or a junction (irregular on Windows) is never a plain dir.
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: staging dir is not a plain directory", ErrStagedPathRefused)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: staging dir is accessible to other users", ErrStagedPathRefused)
	}
	return dir, nil
}

// closeStaging marks staging closed once any in-flight stage has finished, so
// no stage can write after teardown starts removing the dir.
func (s *cobrowseSession) closeStaging() {
	s.stageMu.Lock()
	s.stageClosed = true
	s.stageMu.Unlock()
}

// removeStagingDir deletes staged files (bounded retries; a Windows file lock
// can delay it) and reports failure. Teardown refuses to seal a persistent
// profile while it fails.
func (s *cobrowseSession) removeStagingDir() error {
	if s.profile == "" {
		return nil
	}
	if err := removeSessionDir(s.stagingDirPath()); err != nil {
		return stagingFailure("remove staging dir", err)
	}
	return nil
}

// StageFile writes data into the session's staging dir as name. It is
// create-only (an existing name is refused, never overwritten) and never
// follows a symlink at the leaf. It does not touch the browser, so it works
// while a human is driving; it needs only a session that exists.
func (m *CobrowseSessionManager) StageFile(id, name string, data []byte) (StagedFile, error) {
	if err := ValidateStagedName(name); err != nil {
		return StagedFile{}, err
	}
	if len(data) == 0 {
		return StagedFile{}, errors.New("staged file is empty")
	}
	if len(data) > MaxCobrowseStagedFileBytes {
		return StagedFile{}, ErrStagedTooLarge
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return StagedFile{}, err
	}
	s.mu.Lock()
	running := s.proc != nil
	s.mu.Unlock()
	if !running {
		return StagedFile{}, ErrNotStarted
	}
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	if s.stageClosed {
		return StagedFile{}, ErrNotStarted
	}
	dir, err := s.ensureStagingDir()
	if err != nil {
		return StagedFile{}, err
	}
	if free, ferr := availableStageBytes(dir); ferr == nil && free < uint64(len(data))+minFreeAfterStageBytes {
		return StagedFile{}, errors.New("staging: not enough free disk space on the node")
	}
	if ents, err := os.ReadDir(dir); err == nil && len(ents) >= MaxCobrowseStagedFiles {
		return StagedFile{}, fmt.Errorf("session already holds %d staged files", MaxCobrowseStagedFiles)
	}
	// O_EXCL refuses an existing entry of any kind, including a symlink, so
	// nothing is overwritten and nothing is followed.
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return StagedFile{}, fmt.Errorf("%w: %q is already staged", ErrStagedPathRefused, name)
		}
		return StagedFile{}, stagingFailure("create staged file", err)
	}
	sum := sha256.Sum256(data)
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(filepath.Join(dir, name))
		return StagedFile{}, stagingFailure("write staged file", errors.Join(werr, cerr))
	}
	out := StagedFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	log.Printf("[cobrowse] audit stage_file session=%s name=%q size=%d sha256=%s", id, out.Name, out.Size, out.SHA256)
	return out, nil
}

// resolveStagedFile returns the absolute path of a staged file after proving
// it is a regular file directly inside this session's staging dir, with no
// symlink anywhere on the way, and its size and hash for the audit line.
func (s *cobrowseSession) resolveStagedFile(name string) (string, StagedFile, error) {
	if err := ValidateStagedName(name); err != nil {
		return "", StagedFile{}, err
	}
	refuse := func(why string) (string, StagedFile, error) {
		return "", StagedFile{}, fmt.Errorf("%w: %s", ErrStagedPathRefused, why)
	}
	dir, err := s.ensureStagingDir()
	if err != nil {
		return "", StagedFile{}, err
	}
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return refuse(fmt.Sprintf("%q is not staged", name))
		}
		return "", StagedFile{}, stagingFailure("inspect staged file", err)
	}
	if !info.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	// The staging dir must be exactly where this session's dir says, with no
	// symlink on the way (the session dir itself may sit under a link).
	realSession, err := filepath.EvalSymlinks(s.profile)
	if err != nil {
		return "", StagedFile{}, stagingFailure("resolve session dir", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", StagedFile{}, stagingFailure("resolve staging dir", err)
	}
	if realDir != filepath.Join(realSession, cobrowseStagingDirName) {
		return refuse("staging dir is not this session's")
	}
	abs, err := filepath.Abs(filepath.Join(realDir, name))
	if err != nil {
		return "", StagedFile{}, stagingFailure("resolve staged file", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", StagedFile{}, stagingFailure("open staged file", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", StagedFile{}, stagingFailure("read staged file", err)
	}
	return abs, StagedFile{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// FileUploadPageFenceV1 is the capability value reported in status: set_file_input
// requires expected_url and refuses unless the pinned page's live URL equals it.
const FileUploadPageFenceV1 = "expected_url_v1"

// setFilesIfPageIs is the last step of set_file_input: it reads the pinned
// page's live URL over the action's own connection and hands the browser the
// files only when it equals expectedURL exactly (query and fragment included).
// A mismatch is ErrTargetChanged with nothing dispatched. The URL is never put
// in an error or a log.
func setFilesIfPageIs(c cdpCaller, expectedURL string, params map[string]any, eff *ActionEffects) error {
	val, err := evaluateOn(c, "location.href", false)
	if err != nil {
		return err
	}
	if cur, _ := val.(string); cur == "" || cur != expectedURL {
		return ErrTargetChanged
	}
	eff.Dispatched = true
	if _, err := c.Call("DOM.setFileInputFiles", params); err != nil {
		return err
	}
	eff.FilesSet = 1
	return nil
}

// SetFileInput fills one file input with one staged file. Exactly one of
// selector or backendNodeID names the input (backendNodeID is the one a
// pending file chooser reports). The staged file is resolved and hashed
// before any CDP call, so a refused path never reaches the browser.
func (m *CobrowseSessionManager) SetFileInput(id, selector string, backendNodeID int, stagedName, expectedURL string) (CobrowseSessionStatus, error) {
	if expectedURL == "" {
		return CobrowseSessionStatus{}, errors.New("set_file_input requires 'expected_url'")
	}
	if (selector == "") == (backendNodeID == 0) {
		return CobrowseSessionStatus{}, errors.New("set_file_input requires exactly one of 'selector' or 'backend_node_id'")
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	// A node id is only accepted for the chooser the page actually opened:
	// the caller cannot aim a staged file at an arbitrary node.
	if backendNodeID != 0 {
		s.mu.Lock()
		match := s.chooser != nil && s.chooser.BackendNodeID == backendNodeID
		s.mu.Unlock()
		if !match {
			return CobrowseSessionStatus{}, errors.New("no pending file chooser for that backend_node_id")
		}
	}
	path, file, err := s.resolveStagedFile(stagedName)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.runAction(b, p, "set_file_input", func(r *actionRun) error {
		params := map[string]any{"files": []string{path}}
		var nodeID int
		if selector != "" {
			res, err := r.conn.Call("Runtime.evaluate", map[string]any{
				"expression": fmt.Sprintf("document.querySelector(%s)", jsStringLiteral(selector)),
			})
			if err != nil {
				return err
			}
			obj, _ := res["result"].(map[string]any)
			objectID, _ := obj["objectId"].(string)
			if objectID == "" {
				return fmt.Errorf("no element matches selector %q", selector)
			}
			info, err := r.conn.Call("Runtime.callFunctionOn", map[string]any{
				"objectId":            objectID,
				"returnByValue":       true,
				"functionDeclaration": `function(){return {file: this instanceof HTMLInputElement && this.type === "file", disabled: !!this.disabled}}`,
			})
			if err != nil {
				return err
			}
			val, _ := info["result"].(map[string]any)
			v, _ := val["value"].(map[string]any)
			if isFile, _ := v["file"].(bool); !isFile {
				return fmt.Errorf("selector %q does not match a file input", selector)
			}
			if disabled, _ := v["disabled"].(bool); disabled {
				return fmt.Errorf("file input %q is disabled", selector)
			}
			if d, err := r.conn.Call("DOM.describeNode", map[string]any{"objectId": objectID}); err == nil {
				if node, ok := d["node"].(map[string]any); ok {
					if n, ok := node["backendNodeId"].(float64); ok {
						nodeID = int(n)
					}
				}
			} else {
				// The node cannot be identified, so a pending chooser could
				// not be matched to it: do not leave a stale one behind.
				s.clearChooser()
			}
			params["objectId"] = objectID
		} else {
			nodeID = backendNodeID
			params["backendNodeId"] = backendNodeID
		}
		// The element lookup awaits the page, during which a human can take
		// control or the active page can change: check after the last await
		// and before the browser is handed the file.
		if err := r.fence(); err != nil {
			return err
		}
		if err := setFilesIfPageIs(r.conn, expectedURL, params, r.eff); err != nil {
			// Never return the node's absolute path to the platform.
			return errors.New(strings.ReplaceAll(err.Error(), path, file.Name))
		}
		s.clearChooserFor(nodeID)
		log.Printf("[cobrowse] audit set_file_input session=%s name=%q size=%d sha256=%s", id, file.Name, file.Size, file.SHA256)
		return nil
	})
}
