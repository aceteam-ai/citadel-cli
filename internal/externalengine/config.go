// Package externalengine owns the explicit, node-local vLLM adoption record.
// The record is deliberately separate from the managed module lockfile.
package externalengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/google/uuid"
)

const FileName = "external-engine-v1.json"

type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"host_port"`
}

func (e Endpoint) BaseURL() string { return "http://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

type Config struct {
	Version   int      `json:"version"`
	Mode      string   `json:"mode"` // adopted or detached
	Endpoint  Endpoint `json:"endpoint"`
	Model     string   `json:"model"`
	Revision  string   `json:"revision"`
	RequestID string   `json:"request_id"`
	NodeID    string   `json:"node_id"`
	ConfigRef string   `json:"config_ref"`
}

func ParseHost(host string) (string, error) {
	if host == "localhost" {
		return "127.0.0.1", nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" || addr.String() != host || addr.Is4In6() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return "", fmt.Errorf("host must be a canonical local IP literal or localhost")
	}
	if !addr.IsLoopback() && (!addr.IsGlobalUnicast() || isMetadataAddress(addr)) {
		return "", fmt.Errorf("host is not an allowed local address")
	}
	return host, nil
}

func isMetadataAddress(addr netip.Addr) bool {
	return netip.MustParsePrefix("169.254.0.0/16").Contains(addr) || netip.MustParsePrefix("fe80::/10").Contains(addr)
}

func ValidateEndpoint(e Endpoint, localOnly bool) (Endpoint, error) {
	return validateEndpoint(e, localOnly, net.InterfaceAddrs)
}

func validateEndpoint(e Endpoint, localOnly bool, localAddrs func() ([]net.Addr, error)) (Endpoint, error) {
	host, err := ParseHost(e.Host)
	if err != nil {
		return Endpoint{}, err
	}
	if e.Port < 1 || e.Port > 65535 {
		return Endpoint{}, fmt.Errorf("host_port must be between 1 and 65535")
	}
	e.Host = host
	if localOnly {
		addr := netip.MustParseAddr(host)
		if !addr.IsLoopback() {
			ifaces, err := localAddrs()
			if err != nil {
				return Endpoint{}, fmt.Errorf("list local interfaces: %w", err)
			}
			found := false
			for _, iface := range ifaces {
				prefix, err := netip.ParsePrefix(iface.String())
				if err == nil && prefix.Addr().Unmap() == addr {
					found = true
					break
				}
			}
			if !found {
				return Endpoint{}, fmt.Errorf("host is not assigned to this node")
			}
		}
	}
	return e, nil
}

func ValidateRevision(rev string) (*big.Int, error) {
	if rev == "" || len(rev) > 128 || (len(rev) > 1 && rev[0] == '0') || strings.Trim(rev, "0123456789") != "" {
		return nil, fmt.Errorf("revision must be a canonical decimal string")
	}
	n, ok := new(big.Int).SetString(rev, 10)
	if !ok || n.Sign() <= 0 {
		return nil, fmt.Errorf("revision must be positive")
	}
	return n, nil
}

func Ref(c Config) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1\x00%s\x00%s\x00%d\x00%s\x00%s", c.Mode, c.Endpoint.Host, c.Endpoint.Port, c.Model, c.Revision)))
	return "external-vllm-sha256:" + hex.EncodeToString(sum[:])
}

func Validate(c Config, localOnly bool) (Config, error) {
	if c.Version != 1 || (c.Mode != "adopted" && c.Mode != "detached") {
		return Config{}, fmt.Errorf("unsupported external engine version or mode")
	}
	if _, err := ValidateRevision(c.Revision); err != nil {
		return Config{}, err
	}
	if id, err := uuid.Parse(c.RequestID); err != nil || id.String() != c.RequestID || c.NodeID == "" {
		return Config{}, fmt.Errorf("request_id must be a canonical UUID and node_id is required")
	}
	if c.NodeID == "0" || c.NodeID[0] == '0' || strings.Trim(c.NodeID, "0123456789") != "" {
		return Config{}, fmt.Errorf("node_id must be a canonical positive numeric identifier")
	}
	if c.Mode == "adopted" {
		var err error
		c.Endpoint, err = ValidateEndpoint(c.Endpoint, localOnly)
		if err != nil {
			return Config{}, err
		}
		if c.Model == "" || len(c.Model) > 512 || strings.TrimSpace(c.Model) != c.Model {
			return Config{}, fmt.Errorf("model must be an exact nonempty identifier")
		}
	}
	c.ConfigRef = Ref(c)
	return c, nil
}

func Path(dir string) string { return filepath.Join(dir, FileName) }

func load(dir string, localOnly bool) (*Config, error) {
	b, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 16*1024 {
		return nil, fmt.Errorf("external engine config too large")
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	validated, err := Validate(c, localOnly)
	if err != nil {
		return nil, err
	}
	if c.ConfigRef != "" && c.ConfigRef != validated.ConfigRef {
		return nil, fmt.Errorf("external engine config_ref mismatch")
	}
	return &validated, nil
}

// Load validates the persisted record for use by the current process. Adopted
// non-loopback endpoints must still be assigned to this node at load time.
func Load(dir string) (*Config, error) { return load(dir, true) }

// LoadPersisted validates a record for reconciliation without requiring its
// former endpoint to remain assigned. This lets a newer desired revision
// replace or detach an otherwise valid record after an interface-address
// change; the new adopted candidate still receives strict local validation.
func LoadPersisted(dir string) (*Config, error) { return load(dir, false) }

// Save uses a restrictive temporary file, fsync, rename and parent fsync.
// A previous-state copy is retained for operator recovery.
func Save(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	previous, err := os.ReadFile(Path(dir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) != 0 {
		if err := atomicWrite(Path(dir)+".bak", previous); err != nil {
			return err
		}
	}
	return atomicWrite(Path(dir), b)
}

// Restore puts the pre-action record back after a failed re-exec. Nil means
// this node had no explicit record before the attempted action.
func Restore(dir string, previous *Config) error {
	if previous != nil {
		return Save(dir, *previous)
	}
	if err := os.Remove(Path(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func atomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".external-engine-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Probe requires the exact model. It never follows redirects or environment proxies.
func Probe(ctx context.Context, e Endpoint, model string) error {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL()+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("models endpoint returned %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		return fmt.Errorf("decode models: %w", err)
	}
	for _, m := range body.Data {
		if m.ID == model {
			return nil
		}
	}
	return fmt.Errorf("requested model %q is not served", model)
}

var active struct {
	sync.Once
	config *Config
	err    error
}

// Current snapshots the startup state. A successful job only changes disk;
// the current process keeps its prior routing until re-exec succeeds.
func Current() (*Config, error) {
	active.Do(func() { active.config, active.err = Load(network.GetNodeConfigDir()) })
	return active.config, active.err
}

func VLLMEndpoint() (Endpoint, bool, error) {
	c, err := Current()
	if err != nil {
		return Endpoint{}, false, err
	}
	if c != nil {
		if c.Mode == "detached" {
			return Endpoint{}, false, nil
		}
		return c.Endpoint, true, nil
	}
	host := os.Getenv("CITADEL_VLLM_HOST")
	if host == "" {
		// Preserve the legacy URL spelling and dual-stack resolver behavior when
		// no explicit host is configured.
		return Endpoint{Host: "localhost", Port: services.VLLMHostPort}, true, nil
	}
	endpoint, err := ValidateEndpoint(Endpoint{Host: host, Port: services.VLLMHostPort}, true)
	if err != nil {
		return Endpoint{}, false, err
	}
	return endpoint, true, nil
}

func VLLMBaseURL() string {
	e, enabled, err := VLLMEndpoint()
	if err != nil || !enabled {
		return ""
	}
	return e.BaseURL()
}
