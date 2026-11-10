package hpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Platform routes used by the HPC node (aceteam#12717 contract).
const (
	EnrollPath     = "/api/fabric/hpc/enroll"
	DeregisterPath = "/api/fabric/device-auth/deregister"
)

const (
	maxTokenFileBytes = 4096
	maxResponseBytes  = 1 << 20
)

// NodeIDPattern is the platform's API-only node id shape.
var NodeIDPattern = regexp.MustCompile(`^hpc-[0-9a-f]{32}$`)

// ErrTokenUnavailable is the platform's 409: the token was replayed, expired,
// revoked, or its minter lost the manager role. A new token is needed.
var ErrTokenUnavailable = errors.New("enrollment token unavailable (already used, expired or revoked); mint a new one")

// ReadTokenFile reads a single-use enrollment token and removes the file. The
// file must be a regular file (not a symlink) owned by uid with no group or
// other permissions. A file that fails those checks is left in place and
// refused. A token that was read is never left on disk: if removal fails the
// read fails too.
func ReadTokenFile(path string, uid int) (string, error) {
	if err := CheckTokenFile(path, uid); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("enrollment token file: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	f.Close()
	if rmErr := os.Remove(path); rmErr != nil {
		return "", fmt.Errorf("remove enrollment token file: %w", rmErr)
	}
	if readErr != nil {
		return "", fmt.Errorf("read enrollment token file: %w", readErr)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > 128 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("enrollment token file does not hold a single token")
	}
	return token, nil
}

// CheckTokenFile applies ReadTokenFile's custody checks without reading or
// removing the file, so a launcher can refuse early on a login node.
func CheckTokenFile(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("enrollment token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("enrollment token file must be a regular file, not a symlink or device")
	}
	owner, err := ownerUID(info)
	if err != nil {
		return err
	}
	if owner != uid {
		return fmt.Errorf("enrollment token file is owned by uid %d, not %d", owner, uid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("enrollment token file has group or other permissions (%#o); expected 0600", info.Mode().Perm())
	}
	if info.Size() > maxTokenFileBytes {
		return errors.New("enrollment token file is too large")
	}
	return nil
}

// EnrollResponse is the platform's 200 body.
type EnrollResponse struct {
	NodeID         string `json:"node_id"`
	OrgID          string `json:"org_id"`
	DeviceAPIToken string `json:"device_api_token"`
	APIBaseURL     string `json:"api_base_url"`
	LeafPEM        string `json:"leaf_pem"`
	ChainPEM       string `json:"chain_pem"`
	NodeUID        string `json:"node_uid"`
	ExpiresAt      string `json:"expires_at"`
}

// Validate checks the fields the node depends on.
func (r EnrollResponse) Validate() error {
	if !NodeIDPattern.MatchString(r.NodeID) {
		return errors.New("enrollment response has an invalid node id")
	}
	if strings.TrimSpace(r.OrgID) == "" || strings.TrimSpace(r.DeviceAPIToken) == "" {
		return errors.New("enrollment response is missing the organization or credential")
	}
	if err := checkPlatformURL(r.APIBaseURL); err != nil {
		return fmt.Errorf("enrollment response api_base_url: %w", err)
	}
	return nil
}

// checkPlatformURL requires https, allowing plain http only to a loopback
// host (local fakes and tests).
func checkPlatformURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("not an absolute URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return errors.New("must use https")
}

// NewHTTPClient returns the client used for enrollment and deregistration.
// It honors HTTPS_PROXY / HTTP_PROXY / NO_PROXY and never follows redirects,
// so the token and credential are sent only to the URL that was configured.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Client talks to the platform's HPC routes.
type Client struct {
	HTTP *http.Client
}

func (c Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return NewHTTPClient()
}

// Enroll redeems the single-use token. Errors never contain the token.
func (c Client) Enroll(ctx context.Context, baseURL, token, csrPEM string, labels Labels) (EnrollResponse, error) {
	if err := checkPlatformURL(baseURL); err != nil {
		return EnrollResponse{}, fmt.Errorf("enrollment URL: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"enrollment_token": token,
		"csr_pem":          csrPEM,
		"labels":           labels.Map(),
	})
	if err != nil {
		return EnrollResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+EnrollPath, bytes.NewReader(body))
	if err != nil {
		return EnrollResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return EnrollResponse{}, fmt.Errorf("enrollment request failed: %w", scrubURLError(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusConflict:
		return EnrollResponse{}, ErrTokenUnavailable
	case http.StatusBadRequest:
		return EnrollResponse{}, errors.New("the platform rejected the enrollment request (check the Slurm labels and the CSR)")
	case http.StatusForbidden:
		return EnrollResponse{}, errors.New("the organization's plan or feature settings do not allow HPC enrollment")
	case http.StatusServiceUnavailable:
		return EnrollResponse{}, errors.New("the platform could not complete enrollment; the token may already be burned, mint a new one")
	default:
		return EnrollResponse{}, fmt.Errorf("enrollment failed with HTTP %d", resp.StatusCode)
	}
	if len(data) > maxResponseBytes {
		return EnrollResponse{}, errors.New("enrollment response is too large")
	}
	var out EnrollResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return EnrollResponse{}, errors.New("enrollment response is not valid JSON")
	}
	if err := out.Validate(); err != nil {
		return EnrollResponse{}, err
	}
	return out, nil
}

// Deregister removes this node with its own bound credential. The server
// ignores the body and removes exactly the bound node; wantNodeID guards
// against a response for a different node.
func (c Client) Deregister(ctx context.Context, apiBaseURL, deviceToken, wantNodeID string) error {
	if err := checkPlatformURL(apiBaseURL); err != nil {
		return fmt.Errorf("deregister URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiBaseURL, "/")+DeregisterPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("deregister request failed: %w", scrubURLError(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deregister failed with HTTP %d", resp.StatusCode)
	}
	var result struct {
		Success bool   `json:"success"`
		NodeID  string `json:"node_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !result.Success {
		return errors.New("the platform did not confirm deregistration")
	}
	if result.NodeID != "" && result.NodeID != wantNodeID {
		return errors.New("the platform deregistered a different node")
	}
	return nil
}

// scrubURLError drops the URL from a transport error (it carries no secret,
// but keeps messages short and stable).
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
