// Package whatsapp manages the WhatsApp (Baileys) bridge community module:
// generating its admin secret, talking to its REST control plane to mint a
// tenant, polling health, and fetching the pairing QR. The logic is shared by
// the `citadel whatsapp` CLI command and the control-center TUI page so both
// surface an identical deploy -> pair -> connect flow.
//
// The bridge (sunapi386/whatsapp-bridge) is multi-tenant: one shared bridge
// serves many tenants and the per-tenant `X-API-Key` is the tenant selector.
// The `/admin/*` control plane is guarded by a separate `X-Admin-Key`
// (ADMIN_API_KEY) which the operator holds. The data-plane key handed to the
// aceteam `whatsapp_connect` MCP tool is the per-tenant `wab_...` key minted
// here, NOT the admin key.
package whatsapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
)

// ServiceName is the canonical catalog/service name for the bridge module.
const ServiceName = "whatsapp-bridge"

// DefaultPort is the bridge's published REST port.
const DefaultPort = 8080

// EnvFileName is the env file the CLI writes next to the compose file. It holds
// the generated ADMIN_API_KEY and other compose variables. It is written 0600
// because it contains the admin secret.
const EnvFileName = "whatsapp-bridge.env"

// httpTimeout bounds every bridge HTTP call so a wedged bridge never hangs the
// CLI or the TUI's poll loop.
const httpTimeout = 10 * time.Second

// Client talks to a running bridge over HTTP. BaseURL is the bridge's reachable
// address (e.g. http://100.64.0.5:8080); AdminKey is the X-Admin-Key for the
// /admin/* control plane.
type Client struct {
	BaseURL  string
	AdminKey string
	HTTP     *http.Client
}

// NewClient builds a Client for a bridge at baseURL with the given admin key.
func NewClient(baseURL, adminKey string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		AdminKey: adminKey,
		HTTP:     &http.Client{Timeout: httpTimeout},
	}
}

// Tenant is the response from minting a tenant via POST /admin/tenants. APIKey
// is the per-tenant data-plane key (`wab_...`) the operator passes to
// whatsapp_connect. It is shown once and not retrievable later.
type Tenant struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	APIKey string `json:"api_key"`
	QRURL  string `json:"qr_url"`
	Note   string `json:"note"`
}

// Health is the response from GET /health (per-tenant, X-API-Key auth).
type Health struct {
	Status    string `json:"status"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
}

// GenerateAdminKey returns a strong random hex secret suitable for ADMIN_API_KEY.
func GenerateAdminKey() (string, error) {
	b := make([]byte, 24) // 192 bits
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate admin key: %w", err)
	}
	return "wab_admin_" + hex.EncodeToString(b), nil
}

// Root pings the unauthenticated GET / endpoint, which returns 200 once the
// bridge process is up. Used as the readiness probe after `docker compose up`.
func (c *Client) Root(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge root returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// WaitReady polls GET / until it returns 200 or the timeout elapses.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		callCtx, cancel := context.WithTimeout(ctx, httpTimeout)
		err := c.Root(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("bridge did not become ready within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("bridge did not become ready within %s", timeout)
}

// CreateTenant mints a tenant (and its data-plane API key) via the admin control
// plane. proxyURL is optional (per-tenant egress proxy); pass "" for none.
func (c *Client) CreateTenant(ctx context.Context, name, proxyURL string) (*Tenant, error) {
	if c.AdminKey == "" {
		return nil, fmt.Errorf("admin key is required to create a tenant")
	}
	payload := map[string]string{"name": name}
	if proxyURL != "" {
		payload["proxy_url"] = proxyURL
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/tenants", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", c.AdminKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create tenant failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var t Tenant
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode tenant response: %w", err)
	}
	if t.APIKey == "" {
		return nil, fmt.Errorf("bridge returned an empty api_key")
	}
	return &t, nil
}

// ListTenants returns the existing tenants from GET /admin/tenants. The admin
// list never exposes api_key, so this is for status/QR re-fetch by tenant id.
func (c *Client) ListTenants(ctx context.Context) ([]map[string]any, error) {
	if c.AdminKey == "" {
		return nil, fmt.Errorf("admin key is required to list tenants")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/admin/tenants", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Admin-Key", c.AdminKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list tenants failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode tenants: %w", err)
	}
	return out, nil
}

// Health fetches GET /health for a tenant using its data-plane api key.
func (c *Client) Health(ctx context.Context, apiKey string) (*Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var h Health
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("decode health: %w", err)
	}
	return &h, nil
}

// QRString fetches the raw QR payload from GET /qr.txt for a tenant. The bridge
// returns an empty string when the tenant is already logged in (no QR needed).
func (c *Client) QRString(ctx context.Context, apiKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/qr.txt", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qr fetch failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return strings.TrimSpace(string(data)), nil
}

// RenderQRANSI renders a QR payload as a half-block ANSI string suitable for a
// real terminal (used by the CLI). Returns "" for an empty payload.
func RenderQRANSI(payload string) string {
	if payload == "" {
		return ""
	}
	var buf bytes.Buffer
	qrterminal.GenerateWithConfig(payload, qrterminal.Config{
		Writer:         &buf,
		Level:          qrterminal.L,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      2,
	})
	return buf.String()
}

// RenderQRBlocks renders a QR payload using plain full-block characters with no
// ANSI escapes, so it displays correctly inside a tview TextView (the TUI).
// Returns "" for an empty payload.
func RenderQRBlocks(payload string) string {
	if payload == "" {
		return ""
	}
	var buf bytes.Buffer
	qrterminal.GenerateWithConfig(payload, qrterminal.Config{
		Writer:    &buf,
		Level:     qrterminal.L,
		BlackChar: "  ", // QR "black" module -> two spaces (inverted for light terminals)
		WhiteChar: "██", // QR "white"/quiet -> filled blocks
		QuietZone: 2,
	})
	return buf.String()
}
