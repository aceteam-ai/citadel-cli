//go:build linux

package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/artifact"
)

func pinnedRangePayload(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	identity, err := artifact.Capture(f)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("sha256:%x", h.Sum(nil))
	generation, err := identity.Generation(hash)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"path": path, "offset": "0", "length": "4", "expected_artifact_version": "1", "expected_sha256": hash, "expected_bytes": strconv.FormatInt(identity.Bytes, 10), "expected_generation": generation}
}

func conditionalResult(t *testing.T, h *FileReadBytesRangeV2Handler, p map[string]string) map[string]any {
	t.Helper()
	output, err := h.Execute(JobContext{}, makeJob(p))
	if err != nil {
		t.Fatalf("execute conditional range: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertPrecondition(t *testing.T, result map[string]any) {
	t.Helper()
	if len(result) != 2 || result["contract_version"] != float64(2) || result["outcome"] != "precondition_failed" {
		t.Fatalf("refusal disclosed bytes or replacement metadata: %+v", result)
	}
}

func assertRangeOK(t *testing.T, r map[string]any, p map[string]string, want []byte, total, offset int64) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(fmt.Sprint(r["content"]))
	identity, ok := r["artifact"].(map[string]any)
	if err != nil || !bytes.Equal(data, want) || len(r) != 9 || r["contract_version"] != float64(2) || r["outcome"] != "ok" || r["encoding"] != "base64" || r["offset"] != float64(offset) || r["size"] != float64(len(want)) || r["total_size"] != float64(total) || r["eof"] != (offset+int64(len(want)) == total) || !ok || len(identity) != 4 || identity["version"] != float64(1) || identity["sha256"] != p["expected_sha256"] || identity["bytes"] != float64(total) || identity["generation"] != p["expected_generation"] {
		t.Fatalf("invalid conditional success: %+v data=%q err=%v", r, data, err)
	}
}

func TestConditionalRangeRepeatedPlaybackAndEOF(t *testing.T) {
	ws := t.TempDir()
	fixture := []byte("first-middle-final-content")
	path := writeTestFile(t, ws, "video", string(fixture))
	p := pinnedRangePayload(t, path)
	h := NewFileReadBytesRangeV2Handler(ws)
	assembled := make([]byte, len(fixture))
	for _, offset := range []int{12, 0, 24, 4, 16, 8, 20, 0, 12} {
		p["offset"] = strconv.Itoa(offset)
		p["length"] = "4"
		end := offset + 4
		if end > len(fixture) {
			end = len(fixture)
		}
		r := conditionalResult(t, h, p)
		assertRangeOK(t, r, p, fixture[offset:end], int64(len(fixture)), int64(offset))
		copy(assembled[offset:], fixture[offset:end])
	}
	if !bytes.Equal(assembled, fixture) {
		t.Fatalf("ranges did not reassemble: %q", assembled)
	}
	n := int64(len(fixture))
	p["offset"] = fmt.Sprint(n - 1)
	assertRangeOK(t, conditionalResult(t, h, p), p, fixture[len(fixture)-1:], n, n-1)
	for _, offset := range []int64{n, n + 1, artifact.MaxSafeInteger} {
		p["offset"] = fmt.Sprint(offset)
		r := conditionalResult(t, h, p)
		if len(r) != 5 || r["outcome"] != "range_not_satisfiable" || r["contract_version"] != float64(2) || r["offset"] != float64(offset) || r["total_size"] != float64(n) {
			t.Fatalf("EOF envelope: %+v", r)
		}
		identity := r["artifact"].(map[string]any)
		if identity["generation"] != p["expected_generation"] || identity["bytes"] != float64(n) {
			t.Fatalf("unverified EOF identity: %+v", identity)
		}
	}
	empty := writeTestFile(t, ws, "empty", "")
	zero := conditionalResult(t, h, pinnedRangePayload(t, empty))
	if zero["outcome"] != "range_not_satisfiable" || zero["total_size"] != float64(0) {
		t.Fatalf("empty EOF: %+v", zero)
	}
}

// rangeProbe wraps a real descriptor and lets tests deterministically mutate
// after opening or inside ReadAt, while recording the actual bounded I/O.
type rangeProbe struct {
	*os.File
	afterStat                       func(int)
	beforeRead, afterRead           func()
	stats, reads, volume, maxBuffer int
	short                           bool
	readError                       error
	statInfo                        func(os.FileInfo) os.FileInfo
}

func (f *rangeProbe) Stat() (os.FileInfo, error) {
	info, err := f.File.Stat()
	f.stats++
	if f.afterStat != nil {
		f.afterStat(f.stats)
	}
	if f.statInfo != nil && err == nil {
		info = f.statInfo(info)
	}
	return info, err
}
func (f *rangeProbe) ReadAt(p []byte, off int64) (int, error) {
	f.reads++
	f.volume += len(p)
	if len(p) > f.maxBuffer {
		f.maxBuffer = len(p)
	}
	if f.beforeRead != nil {
		f.beforeRead()
	}
	if f.readError != nil {
		return 0, f.readError
	}
	n, err := f.File.ReadAt(p, off)
	if f.afterRead != nil {
		f.afterRead()
	}
	if f.short && n > 0 {
		return n - 1, nil
	}
	return n, err
}

func TestConditionalRangeLargeFileBoundedIO(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "large.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const total int64 = 64<<20 + 5
	if err := f.Truncate(total); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("FINAL"), total-5); err != nil {
		t.Fatal(err)
	}
	f.Close()
	p := pinnedRangePayload(t, path)
	h := NewFileReadBytesRangeV2Handler(ws)
	var probe *rangeProbe
	h.openFile = func(path string) (rangeDescriptor, error) {
		f, err := os.Open(path)
		probe = &rangeProbe{File: f}
		return probe, err
	}
	p["offset"] = fmt.Sprint(total - 5)
	p["length"] = "8"
	p["max_bytes"] = "8"
	result := conditionalResult(t, h, p)
	assertRangeOK(t, result, p, []byte("FINAL"), total, total-5)
	if probe.reads != 1 || probe.volume != 5 || probe.maxBuffer != 5 {
		t.Fatalf("unbounded I/O: %+v", probe)
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > 1024 || len(fmt.Sprint(result["content"])) != 8 {
		t.Fatal("encoded more than bounded bytes")
	}
	p["offset"] = "0"
	p["length"] = "8388608"
	p["max_bytes"] = "8388608"
	r := conditionalResult(t, h, p)
	if r["size"] != float64(maxReadChunkBytes) || int64(probe.volume) != maxReadChunkBytes || len(fmt.Sprint(r["content"])) != base64.StdEncoding.EncodedLen(int(maxReadChunkBytes)) {
		t.Fatal("8 MiB bound or encoding violated")
	}
	if _, err := NewFileReadBytesHandler(ws).Execute(JobContext{}, makeJob(map[string]string{"path": path, "max_bytes": fmt.Sprint(total)})); err == nil {
		t.Fatal("legacy 50 MiB ceiling raised")
	}
}

func TestConditionalRangeUnexpectedIOUsesErrorPath(t *testing.T) {
	ws := t.TempDir()
	path := writeTestFile(t, ws, "video", "ORIGINAL")
	p := pinnedRangePayload(t, path)
	h := NewFileReadBytesRangeV2Handler(ws)
	want := errors.New("synthetic I/O failure")
	h.openFile = func(path string) (rangeDescriptor, error) {
		f, err := os.Open(path)
		return &rangeProbe{File: f, readError: want}, err
	}
	output, err := h.Execute(JobContext{}, makeJob(p))
	if !errors.Is(err, want) || len(output) != 0 {
		t.Fatalf("I/O error became domain envelope: %s %v", output, err)
	}
}

func TestConditionalRangeReplacementAndRestoredMtimeRewrite(t *testing.T) {
	for _, kind := range []string{"replacement", "in place"} {
		t.Run(kind, func(t *testing.T) {
			ws := t.TempDir()
			path := writeTestFile(t, ws, "video", "ORIGINAL")
			p := pinnedRangePayload(t, path)
			h := NewFileReadBytesRangeV2Handler(ws)
			assertRangeOK(t, conditionalResult(t, h, p), p, []byte("ORIG"), 8, 0)
			info, _ := os.Stat(path)
			if kind == "replacement" {
				other := writeTestFile(t, ws, "new", "REPLACED")
				if err := os.Chtimes(other, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("REPLACED"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				// The real rewrite restores mtime. Supply a deterministic ctime
				// observation so this check does not rely on the filesystem clock
				// advancing between these tightly adjacent syscalls.
				h.openFile = func(path string) (rangeDescriptor, error) {
					f, err := os.Open(path)
					return &rangeProbe{File: f, statInfo: func(current os.FileInfo) os.FileInfo { return advancedCtime(current, info) }}, err
				}
			}
			changed, _ := os.Stat(path)
			if !changed.ModTime().Equal(info.ModTime()) || changed.Size() != info.Size() {
				t.Fatal("test did not preserve size and mtime")
			}
			for _, offset := range []string{"4", "9007199254740991"} {
				p["offset"] = offset
				assertPrecondition(t, conditionalResult(t, h, p))
			}
		})
	}
}

func TestConditionalRangeDuringReadMutationAndShortCount(t *testing.T) {
	for _, kind := range []string{"rewrite", "append", "truncate", "short count", "EOF mutation"} {
		t.Run(kind, func(t *testing.T) {
			ws := t.TempDir()
			path := writeTestFile(t, ws, "video", "ORIGINAL")
			p := pinnedRangePayload(t, path)
			h := NewFileReadBytesRangeV2Handler(ws)
			mutate := func() {
				var err error
				switch kind {
				case "rewrite", "EOF mutation":
					info, _ := os.Stat(path)
					err = os.WriteFile(path, []byte("REPLACED"), 0600)
					if err == nil {
						err = os.Chtimes(path, info.ModTime().Add(-time.Hour), info.ModTime().Add(-time.Hour))
					}
				case "append":
					err = os.WriteFile(path, []byte("ORIGINAL-extra"), 0600)
				case "truncate":
					err = os.Truncate(path, 2)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			h.openFile = func(path string) (rangeDescriptor, error) {
				f, err := os.Open(path)
				probe := &rangeProbe{File: f, short: kind == "short count"}
				if kind == "truncate" {
					probe.beforeRead = mutate
				} else {
					probe.afterRead = mutate
				}
				if kind == "EOF mutation" {
					p["offset"] = "8"
					probe.afterStat = func(n int) {
						if n == 1 {
							mutate()
						}
					}
				}
				return probe, err
			}
			// EOF selection must be set before validation/open, not by the seam.
			if kind == "EOF mutation" {
				p["offset"] = "8"
			}
			assertPrecondition(t, conditionalResult(t, h, p))
		})
	}
}

func TestConditionalRangeOpenedDescriptorSurvivesPathRace(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "output")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := writeTestFile(t, dir, "video", "ORIGINAL")
	p := pinnedRangePayload(t, path)
	h := NewFileReadBytesRangeV2Handler(ws)
	h.openFile = func(path string) (rangeDescriptor, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		probe := &rangeProbe{File: f}
		// Replacing the containing directory leaves the original file's
		// metadata untouched, making a path reopen observable even with the
		// post-read check present.
		probe.afterStat = func(n int) {
			if n == 1 {
				if err := os.Rename(dir, filepath.Join(ws, "old-output")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, dir, "video", "REPLACED")
			}
		}
		return probe, nil
	}
	r := conditionalResult(t, h, p)
	if r["outcome"] == "precondition_failed" {
		assertPrecondition(t, r)
	} else {
		assertRangeOK(t, r, p, []byte("ORIG"), 8, 0)
	}
	if r["content"] == base64.StdEncoding.EncodeToString([]byte("REPL")) {
		t.Fatal("reopened path and delivered replacement")
	}
}

func TestConditionalRangeInvalidPayloadNeverOpens(t *testing.T) {
	ws := t.TempDir()
	path := writeTestFile(t, ws, "video", "ORIGINAL")
	valid := pinnedRangePayload(t, path)
	for _, field := range []string{"path", "offset", "length", "expected_artifact_version", "expected_sha256", "expected_bytes", "expected_generation"} {
		t.Run("missing "+field, func(t *testing.T) {
			p := map[string]string{}
			for k, v := range valid {
				p[k] = v
			}
			delete(p, field)
			h := NewFileReadBytesRangeV2Handler(ws)
			h.openFile = func(string) (rangeDescriptor, error) {
				t.Fatal("opened missing-identity/range request")
				return nil, nil
			}
			if _, err := h.Execute(JobContext{}, makeJob(p)); err == nil {
				t.Fatal("accepted missing field")
			}
		})
	}
	for field, values := range map[string][]string{
		"offset":                    {"-1", "1.5", "true", "9007199254740992", "9223372036854775808", "+1"},
		"expected_bytes":            {"-1", "1.5", "true", "9007199254740992", "9223372036854775808"},
		"length":                    {"0", "-1", "8388609", "9223372036854775808"},
		"max_bytes":                 {"0", "-1", "3", "8388609", "nope"},
		"expected_artifact_version": {"2", "01"},
		"expected_sha256":           {"sha256:" + strings.Repeat("A", 64), strings.Repeat("a", 64)},
		"expected_generation":       {"size-v1:" + strings.Repeat("a", 64), "linux-stat-v1:" + strings.Repeat("a", 65), "linux-stat-v1:" + strings.Repeat("A", 64)},
		"path":                      {"../outside", "/etc/passwd"},
	} {
		for _, value := range values {
			t.Run(field+"="+value, func(t *testing.T) {
				p := map[string]string{}
				for k, v := range valid {
					p[k] = v
				}
				p[field] = value
				h := NewFileReadBytesRangeV2Handler(ws)
				h.openFile = func(string) (rangeDescriptor, error) { t.Fatal("opened invalid request"); return nil, nil }
				if _, err := h.Execute(JobContext{}, makeJob(p)); err == nil {
					t.Fatal("accepted invalid payload")
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFileReadBytesRangeV2Handler(ws).Execute(JobContext{Ctx: ctx}, makeJob(valid)); err != context.Canceled {
		t.Fatalf("cancellation=%v", err)
	}
}

type missingStatSystem struct{ os.FileInfo }

func (missingStatSystem) Sys() any { return nil }

type replacedStatSystem struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (i replacedStatSystem) Sys() any { return i.stat }
func advancedCtime(current, original os.FileInfo) os.FileInfo {
	stat := *current.Sys().(*syscall.Stat_t)
	stat.Ctim = original.Sys().(*syscall.Stat_t).Ctim
	stat.Ctim.Nsec++
	if stat.Ctim.Nsec == 1e9 {
		stat.Ctim.Sec++
		stat.Ctim.Nsec = 0
	}
	return replacedStatSystem{FileInfo: current, stat: &stat}
}

func TestConditionalRangeUnsupportedObservationAndAuthorizedRoots(t *testing.T) {
	ws := t.TempDir()
	path := writeTestFile(t, ws, "video", "ORIGINAL")
	p := pinnedRangePayload(t, path)
	h := NewFileReadBytesRangeV2Handler(ws)
	h.openFile = func(path string) (rangeDescriptor, error) {
		f, err := os.Open(path)
		return &rangeProbe{File: f, statInfo: func(info os.FileInfo) os.FileInfo { return missingStatSystem{info} }}, err
	}
	if out, err := h.Execute(JobContext{}, makeJob(p)); err == nil || len(out) != 0 {
		t.Fatalf("unsupported observations delivered %s %v", out, err)
	}
	outside := t.TempDir()
	authorized := writeTestFile(t, outside, "video", "ORIGINAL")
	p = pinnedRangePayload(t, authorized)
	h = NewFileReadBytesRangeV2Handler(ws)
	if out, err := h.Execute(JobContext{}, makeJob(p)); err == nil || len(out) != 0 {
		t.Fatal("unauthorized root accepted")
	}
	h.AuthorizedRoots = func() []string { return []string{outside} }
	assertRangeOK(t, conditionalResult(t, h, p), p, []byte("ORIG"), 8, 0)
	escape := filepath.Join(ws, "escape")
	if err := os.Symlink(authorized, escape); err != nil {
		t.Fatal(err)
	}
	h.AuthorizedRoots = nil
	p["path"] = escape
	if _, err := h.Execute(JobContext{}, makeJob(p)); err == nil {
		t.Fatal("symlink escape accepted")
	}
	// The authorized-root opener itself closes validation-to-open escapes.
	validated := writeTestFile(t, ws, "race", "ORIGINAL")
	if err := os.Remove(validated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(authorized, validated); err != nil {
		t.Fatal(err)
	}
	if f, err := h.openAuthorizedFile(validated, nil); err == nil {
		f.Close()
		t.Fatal("raced symlink escaped root opener")
	}
}

func TestConditionalRangeValidSymlinkRootSpellings(t *testing.T) {
	for _, kind := range []string{"absolute symlink workspace", "absolute symlink authorized root", "absolute ancestor alias", "relative ordinary", "relative symlink workspace"} {
		t.Run(kind, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(base, "workspace")
			if err := os.Mkdir(ws, 0700); err != nil {
				t.Fatal(err)
			}
			actual := writeTestFile(t, ws, "video", "ORIGINAL")
			requested := actual
			h := NewFileReadBytesRangeV2Handler(ws)
			switch kind {
			case "absolute symlink workspace", "relative symlink workspace":
				alias := filepath.Join(base, "workspace-link")
				if err := os.Symlink(ws, alias); err != nil {
					t.Fatal(err)
				}
				h.WorkspaceDir = alias
				requested = filepath.Join(alias, "video")
				if kind == "relative symlink workspace" {
					requested = "video"
				}
			case "absolute symlink authorized root":
				root := filepath.Join(base, "authorized")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				actual = writeTestFile(t, root, "video", "ORIGINAL")
				alias := filepath.Join(base, "authorized-link")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				h.AuthorizedRoots = func() []string { return []string{alias} }
				requested = filepath.Join(alias, "video")
			case "absolute ancestor alias":
				alias := filepath.Join(base, "ancestor-link")
				if err := os.Symlink(base, alias); err != nil {
					t.Fatal(err)
				}
				requested = filepath.Join(alias, "workspace", "video")
			case "relative ordinary":
				requested = "video"
			}
			if _, err := ValidateReadPathWithRoots(h.WorkspaceDir, resolveAuthorizedRoots(h.AuthorizedRoots), requested, false); err != nil {
				t.Fatalf("valid spelling refused by existing validator: %v", err)
			}
			p := pinnedRangePayload(t, actual)
			p["path"] = requested
			// Use the real opener and descriptor, not the openFile test seam.
			assertRangeOK(t, conditionalResult(t, h, p), p, []byte("ORIG"), 8, 0)
		})
	}
}

func TestConditionalRangeAliasRetargetBetweenValidationAndOpenRefuses(t *testing.T) {
	for _, kind := range []string{"workspace", "authorized root"} {
		t.Run(kind, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(base, "workspace")
			if err := os.Mkdir(ws, 0700); err != nil {
				t.Fatal(err)
			}
			root := ws
			var roots []string
			if kind == "authorized root" {
				root = filepath.Join(base, "authorized")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				roots = []string{root}
			}
			writeTestFile(t, root, "video", "ORIGINAL")
			outside := filepath.Join(base, "outside")
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, outside, "video", "PRIVATE!")
			alias := filepath.Join(base, "requested-link")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			validated, err := ValidateReadPathWithRoots(ws, roots, filepath.Join(alias, "video"), false)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, alias); err != nil {
				t.Fatal(err)
			}
			h := NewFileReadBytesRangeV2Handler(ws)
			file, err := h.openAuthorizedFile(validated, roots)
			if file != nil {
				file.Close()
				t.Fatal("retargeted alias returned a descriptor outside authorized roots")
			}
			if err == nil {
				t.Fatal("retargeted alias accepted after validation")
			}
		})
	}
}
