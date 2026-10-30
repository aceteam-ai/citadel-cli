// Package containerjobs is the typed container-host boundary. It never accepts
// host paths, shell commands, runtime names or Docker option strings from jobs.
package containerjobs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	Create      = "CONTAINER_CREATE"
	Start       = "CONTAINER_START"
	Stop        = "CONTAINER_STOP"
	Exec        = "CONTAINER_EXEC"
	FileList    = "CONTAINER_FILE_LIST"
	FileRead    = "CONTAINER_FILE_READ"
	FileWrite   = "CONTAINER_FILE_WRITE"
	Usage       = "CONTAINER_USAGE"
	Export      = "CONTAINER_EXPORT_CHUNK"
	Import      = "CONTAINER_IMPORT_CHUNK"
	Destroy     = "CONTAINER_DESTROY"
	FileLimit   = 25 << 20
	OutputLimit = 64 << 10
	ChunkLimit  = 256 << 20
	MaxChunks   = 4096
)

var Types = []string{Create, Start, Stop, Exec, FileList, FileRead, FileWrite, Usage, Export, Import, Destroy}
var ErrNotFound = errors.New("container volume not found")
var ErrInvalid = errors.New("invalid typed container request")
var ErrOwnership = errors.New("container or volume ownership does not match")
var ErrUnavailable = errors.New("container host operation unavailable or unconfirmed")
var ErrBounds = errors.New("container operation exceeds its bounds")
var containerPattern = regexp.MustCompile(`^sandbox-[0-9a-f]{8}$`)
var volumePattern = regexp.MustCompile(`^aceteam-vol-[0-9a-f]{8}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The same flat JSON schema is used by the paired server dispatcher. Transport
// fields are ignored; every field that can cause an effect is validated below.
type Request struct {
	OrgID          string    `json:"org_id"`
	ContainerRef   string    `json:"container_ref,omitempty"`
	VolumeRef      string    `json:"volume_ref,omitempty"`
	Image          string    `json:"image,omitempty"`
	QuotaBytes     int64     `json:"quota_bytes,omitempty"`
	CPUs           float64   `json:"cpus,omitempty"`
	MemoryBytes    int64     `json:"memory_bytes,omitempty"`
	PidsLimit      int64     `json:"pids_limit,omitempty"`
	Network        string    `json:"network,omitempty"`
	Argv           []string  `json:"argv,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty"`
	Path           string    `json:"path,omitempty"`
	ContentB64     string    `json:"content_b64,omitempty"`
	PurgeVolume    bool      `json:"purge_volume,omitempty"`
	TransferID     string    `json:"transfer_id,omitempty"`
	KeyB64         string    `json:"key_b64,omitempty"`
	ChunkURLs      []string  `json:"chunk_urls,omitempty"`
	Manifest       *Manifest `json:"manifest,omitempty"`
	ManifestSHA256 string    `json:"manifest_sha256,omitempty"`
}

type Entry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Bytes     int64  `json:"bytes"`
}
type Chunk struct {
	Index          int    `json:"index"`
	PlaintextBytes int64  `json:"plaintext_bytes"`
	SHA256         string `json:"sha256"`
}

// Field order defines the canonical compact JSON hashed by both implementations.
type Manifest struct {
	Version    int     `json:"version"`
	OrgID      string  `json:"org_id"`
	TransferID string  `json:"transfer_id"`
	Chunks     []Chunk `json:"chunks"`
}
type Result struct {
	ExecTimedOut   bool      `json:"exec_timed_out"`
	Message        string    `json:"message,omitempty"`
	ContainerRef   string    `json:"container_ref,omitempty"`
	VolumeRef      string    `json:"volume_ref,omitempty"`
	Running        *bool     `json:"running,omitempty"`
	ExitCode       int       `json:"exit_code"`
	OutputB64      string    `json:"output_b64,omitempty"`
	Truncated      bool      `json:"truncated"`
	Entries        []Entry   `json:"entries,omitempty"`
	ContentB64     string    `json:"content_b64,omitempty"`
	Bytes          int64     `json:"bytes"`
	UsedBytes      int64     `json:"used_bytes"`
	QuotaBytes     int64     `json:"quota_bytes"`
	Manifest       *Manifest `json:"manifest,omitempty"`
	ManifestSHA256 string    `json:"manifest_sha256,omitempty"`
}

func canonicalUUID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func IsType(s string) bool {
	for _, t := range Types {
		if s == t {
			return true
		}
	}
	return false
}
func validText(s string, max int) bool {
	return utf8.ValidString(s) && len(s) <= max && !strings.ContainsRune(s, 0)
}
func relative(s string, rootOK bool) bool {
	if s == "." {
		return rootOK
	}
	if s == "" || !validText(s, 4096) || strings.HasPrefix(s, "/") || strings.Contains(s, `\`) || path.Clean(s) != s {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == ".." || p == "." || p == "" {
			return false
		}
	}
	return true
}
func validImage(s string) bool {
	return strings.HasPrefix(s, "ghcr.io/aceteam-ai/") && len(s) <= 512 && !strings.ContainsAny(s, " \t\r\n\x00") && !strings.Contains(s, "..") && len(s) > len("ghcr.io/aceteam-ai/")
}
func validURL(s string) bool {
	if len(s) > 8192 {
		return false
	}
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && (u.Port() == "" || u.Port() == "443")
}
func validQuota(n int64) bool { return n >= 16<<20 && n <= math.MaxInt64-(16<<20) && n%4096 == 0 }

func Parse(kind string, raw []byte) (Request, error) {
	var r Request
	if !IsType(kind) || len(raw) > ((FileLimit+2)/3*4)+(2<<20) || json.Unmarshal(raw, &r) != nil {
		return r, ErrInvalid
	}
	if !canonicalUUID(r.OrgID) || r.ContainerRef != "" && !containerPattern.MatchString(r.ContainerRef) || r.VolumeRef != "" && !volumePattern.MatchString(r.VolumeRef) {
		return r, ErrInvalid
	}
	needsVolume := kind == Create || kind == FileList || kind == FileRead || kind == FileWrite || kind == Usage || kind == Export || kind == Import || kind == Destroy && r.PurgeVolume
	needsContainer := kind == Create || kind == Start || kind == Stop || kind == Exec || kind == Usage || kind == Export || kind == Destroy && !r.PurgeVolume
	if needsVolume && r.VolumeRef == "" || needsContainer && r.ContainerRef == "" {
		return r, ErrInvalid
	}
	if kind == Create && (!validImage(r.Image) || !validQuota(r.QuotaBytes) || math.IsNaN(r.CPUs) || math.IsInf(r.CPUs, 0) || r.CPUs <= 0 || r.CPUs > float64(math.MaxInt32) || r.MemoryBytes <= 0 || r.PidsLimit <= 0 || r.PidsLimit > math.MaxInt32 || r.Network != "" && r.Network != "aceteam-net-"+strings.TrimPrefix(r.ContainerRef, "sandbox-")) {
		return r, ErrInvalid
	}
	if kind == Exec {
		if len(r.Argv) == 0 || len(r.Argv) > 256 || r.Argv[0] == "" || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 600 {
			return r, ErrInvalid
		}
		total := 0
		for _, s := range r.Argv {
			if !validText(s, OutputLimit) {
				return r, ErrInvalid
			}
			total += len(s)
		}
		if total > OutputLimit {
			return r, ErrBounds
		}
	}
	if (kind == FileRead || kind == FileWrite || kind == FileList) && !relative(r.Path, kind == FileList) {
		return r, ErrInvalid
	}
	if kind == FileWrite {
		if len(r.ContentB64) > base64.StdEncoding.EncodedLen(FileLimit) {
			return r, ErrBounds
		}
		b, e := base64.StdEncoding.Strict().DecodeString(r.ContentB64)
		if e != nil || len(b) > FileLimit || base64.StdEncoding.EncodeToString(b) != r.ContentB64 {
			return r, ErrInvalid
		}
	}
	if kind == Import || kind == Export {
		key, e := base64.StdEncoding.Strict().DecodeString(r.KeyB64)
		if e != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != r.KeyB64 || !canonicalUUID(r.TransferID) || len(r.ChunkURLs) == 0 || len(r.ChunkURLs) > MaxChunks {
			return r, ErrInvalid
		}
		for _, s := range r.ChunkURLs {
			if !validURL(s) {
				return r, ErrInvalid
			}
		}
		if kind == Import && (!validQuota(r.QuotaBytes) || !hashPattern.MatchString(r.ManifestSHA256) || r.Manifest == nil || r.Manifest.Validate(r.OrgID, r.TransferID, len(r.ChunkURLs)) != nil) {
			return r, ErrInvalid
		}
	}
	return r, nil
}
func (m *Manifest) Validate(org, transfer string, count int) error {
	if m == nil || m.Version != 1 || m.OrgID != org || m.TransferID != transfer || len(m.Chunks) != count || count == 0 || count > MaxChunks {
		return ErrInvalid
	}
	for i, c := range m.Chunks {
		if c.Index != i || c.PlaintextBytes <= 0 || c.PlaintextBytes > ChunkLimit || i < count-1 && c.PlaintextBytes != ChunkLimit || !hashPattern.MatchString(c.SHA256) {
			return ErrInvalid
		}
	}
	return nil
}
