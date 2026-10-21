package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/coordinatorfetch"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileFetchEndpointPath is the dedicated, read-only capability endpoint.
// The coordinator owns HMAC verification, exact org/node/document/object/hash/
// size binding, the ten-minute maximum capability lifetime, and mint/fetch audit.
// Keep this literal in sync with the coordinator; this is not a proxy allowlist.
const FileFetchEndpointPath = coordinatorfetch.FileFetchPath

const (
	fileFetchHardMaxBytes  int64 = coordinatorfetch.MaxBytes
	fileFetchTimeout             = coordinatorfetch.Timeout
	fileFetchMaxTokenBytes       = 4096
)

// FileFetchHandler stages a capability-bound document into the workspace.
// Origin must come from trusted node configuration, never the job payload.
type FileFetchHandler struct {
	WorkspaceDir string
	Origin       func() string
	Disabled     bool
	Enabled      func() bool

	// Private test seams retain the real HTTP transport and filesystem path.
	lookupIP     func(context.Context, string) ([]net.IPAddr, error)
	dialIP       func(context.Context, string, string) (net.Conn, error)
	newTransport func() *http.Transport
	probeStorage func(string) bool

	storageOnce      sync.Once
	storageAvailable bool
}

// NewFileFetchHandler creates a handler with no implicit trusted destination.
func NewFileFetchHandler(workspace string, origin func() string) *FileFetchHandler {
	return &FileFetchHandler{WorkspaceDir: workspace, Origin: origin}
}

type fileFetchError struct{ reason string }

func (e *fileFetchError) Error() string {
	// Only static reasons are used. Never return URL parser, transport, response
	// body or filesystem errors: they can contain tokens or node-local paths.
	return fmt.Sprintf(`{"reason":%q,"message":"FILE_FETCH refused"}`, e.reason)
}

func fileFetchRefusal(reason string) error {
	return &fileFetchError{reason: reason}
}

func fileFetchOrigin(raw string) (*url.URL, error) {
	u, err := coordinatorfetch.Origin(raw)
	if err != nil {
		return nil, fileFetchRefusal("untrusted_origin")
	}
	return u, nil
}

// Configured requires an exact HTTPS origin and working anonymous staging with
// descriptor-based, create-only publication on the workspace filesystem. Worker
// capability advertisement also requires live Files permission.
func (h *FileFetchHandler) Configured() bool {
	if h.Origin == nil {
		return false
	}
	if _, err := fileFetchOrigin(h.Origin()); err != nil {
		return false
	}
	h.storageOnce.Do(func() {
		probe := h.probeStorage
		if probe == nil {
			probe = fileFetchStorageSupported
		}
		h.storageAvailable = probe(h.WorkspaceDir)
	})
	return h.storageAvailable
}

func (h *FileFetchHandler) validateURL(raw string) (*url.URL, error) {
	if h.Origin == nil {
		return nil, fileFetchRefusal("untrusted_origin")
	}
	origin, err := fileFetchOrigin(h.Origin())
	if err != nil {
		return nil, err
	}
	// Bound the job URL to this one canonical endpoint before parsing it.
	if raw != "https://"+origin.Host+FileFetchEndpointPath {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != origin.Scheme || u.Host != origin.Host ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || strings.Contains(raw, "%") ||
		u.Path != FileFetchEndpointPath {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	return u, nil
}

func fileFetchValidateCapability(token string) error {
	if !coordinatorfetch.ValidCapability(token) {
		return fileFetchRefusal("invalid_capability")
	}
	return nil
}

func (h *FileFetchHandler) fetchHooks() coordinatorfetch.Hooks {
	return coordinatorfetch.Hooks{
		LookupIP: h.lookupIP, DialIP: h.dialIP, NewTransport: h.newTransport,
		InterfaceAddrs: func() ([]net.Addr, error) { return interfaceAddrsFn() },
	}
}

func (h *FileFetchHandler) transport(host string) *http.Transport {
	return coordinatorfetch.Transport(host, h.fetchHooks())
}

// Execute accepts only url, capability, dest_path, expected_sha256 and max_bytes (plus
// dispatch metadata ignored by the handler). GET, headers and policy are fixed.
// The opaque capability's signature, expiry and nonce are checked by the endpoint.
func (h *FileFetchHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if (h.Enabled != nil && !h.Enabled()) || (h.Enabled == nil && h.Disabled) {
		return nil, fileFetchRefusal("files_disabled")
	}
	if ctx.Context().Err() != nil || ctx.CancelRequestedNow() {
		return nil, fileFetchRefusal("cancelled_or_timeout")
	}
	u, err := h.validateURL(job.Payload["url"])
	if err != nil {
		return nil, err
	}
	capability := job.Payload["capability"]
	if err := fileFetchValidateCapability(capability); err != nil {
		return nil, err
	}
	dest := job.Payload["dest_path"]
	if dest == "." || !filepath.IsLocal(dest) || strings.ContainsAny(dest, "\\:\x00") || filepath.ToSlash(filepath.Clean(dest)) != dest {
		return nil, fileFetchRefusal("invalid_dest_path")
	}
	wantHash, err := hex.DecodeString(job.Payload["expected_sha256"])
	if err != nil || len(wantHash) != sha256.Size {
		return nil, fileFetchRefusal("invalid_sha256")
	}
	maxBytes, err := strconv.ParseInt(job.Payload["max_bytes"], 10, 64)
	if err != nil || maxBytes <= 0 || maxBytes > fileFetchHardMaxBytes {
		return nil, fileFetchRefusal("invalid_max_bytes")
	}
	if h.WorkspaceDir == "" {
		return nil, fileFetchRefusal("workspace_unavailable")
	}
	workspace, err := filepath.Abs(h.WorkspaceDir)
	if err != nil {
		return nil, fileFetchRefusal("workspace_unavailable")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, fileFetchRefusal("workspace_unavailable")
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return nil, fileFetchRefusal("invalid_dest_path")
	}
	if info, err := root.Lstat(dest); err == nil && !info.Mode().IsRegular() {
		return nil, fileFetchRefusal("invalid_dest_path")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fileFetchRefusal("invalid_dest_path")
	}
	// No name is assigned to the staging inode until its bytes are verified.
	// Keep the descriptor open through publication; never reopen a staging name.
	temp, err := fileFetchOpenStaging(root)
	if err != nil {
		return nil, fileFetchRefusal("staging_unavailable")
	}
	defer temp.Close()

	deadline, cancel := context.WithTimeout(ctx.Context(), fileFetchTimeout)
	defer cancel()
	if ctx.CancelRequested != nil {
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-deadline.Done():
					return
				case <-ticker.C:
					if ctx.CancelRequestedNow() {
						cancel()
						return
					}
				}
			}
		}()
	}
	result, err := coordinatorfetch.Get(deadline, u.Scheme+"://"+u.Host, coordinatorfetch.FileFetch, capability, maxBytes, temp, h.fetchHooks())
	// Cooperative cancellation takes precedence over partial-body classification.
	if deadline.Err() != nil || ctx.CancelRequestedNow() {
		return nil, fileFetchRefusal("cancelled_or_timeout")
	}
	if err != nil {
		var refusal *coordinatorfetch.Refusal
		if errors.As(err, &refusal) {
			return nil, fileFetchRefusal(refusal.Reason())
		}
		return nil, fileFetchRefusal("transfer_failed")
	}
	n, sum := result.Bytes, result.SHA256[:]
	if !bytes.Equal(sum, wantHash) {
		return nil, fileFetchRefusal("sha256_mismatch")
	}
	if err := temp.Sync(); err != nil {
		return nil, fileFetchRefusal("write_failed")
	}
	if deadline.Err() != nil || ctx.CancelRequestedNow() {
		return nil, fileFetchRefusal("cancelled_or_timeout")
	}
	if (h.Enabled != nil && !h.Enabled()) || (h.Enabled == nil && h.Disabled) {
		return nil, fileFetchRefusal("files_disabled")
	}
	if err := fileFetchPublish(root, temp, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fileFetchRefusal("destination_exists")
		}
		return nil, fileFetchRefusal("publish_failed")
	}
	// No URL, token, payload, response headers/body or destination in logs.
	ctx.Log("info", "FILE_FETCH completed (%d bytes)", n)
	return json.Marshal(map[string]any{"path": dest, "bytes": n, "sha256": hex.EncodeToString(sum)})
}

// Capabilities enables the worker's cooperative cancellation watch; cancellation
// aborts the current transfer and leaves any existing destination untouched.
func (*FileFetchHandler) Capabilities() JobCapabilities {
	return JobCapabilities{CooperativeCancel: true}
}

var _ JobHandler = (*FileFetchHandler)(nil)
