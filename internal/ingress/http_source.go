package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpRoutesSource is the production RoutesSource: it fetches the routes map (and
// single-slug on-miss lookups) from the control-plane routes endpoint with a
// bearer token. The bearer is passed in from the env by the cmd layer and never
// logged.
type httpRoutesSource struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPRoutesSource builds a RoutesSource over the given routes endpoint URL
// and bearer token.
func NewHTTPRoutesSource(routesURL, token string, client *http.Client) RoutesSource {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &httpRoutesSource{baseURL: routesURL, token: token, client: client}
}

func (s *httpRoutesSource) Fetch(ctx context.Context, etag string) (int, string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL, nil)
	if err != nil {
		return 0, "", nil, err
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return resp.StatusCode, "", nil, err
	}
	return resp.StatusCode, resp.Header.Get("ETag"), body, nil
}

func (s *httpRoutesSource) FetchOne(ctx context.Context, slug string) (int, []byte, error) {
	u := strings.TrimRight(s.baseURL, "/") + "/" + url.PathEscape(slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// httpAuthorizer is the production Authorizer: it POSTs a gated-app authz check
// to the control plane. The authz URL is derived from the routes URL sibling
// path (routesURL/../authz), matching the DoR's "generic, same bearer" contract.
type httpAuthorizer struct {
	authzURL string
	token    string
	client   *http.Client
}

// NewHTTPAuthorizer builds an Authorizer. authzURL is the full authz endpoint.
func NewHTTPAuthorizer(authzURL, token string, client *http.Client) Authorizer {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &httpAuthorizer{authzURL: authzURL, token: token, client: client}
}

type authzRequest struct {
	Slug   string `json:"slug"`
	Cookie string `json:"cookie"`
}

type authzResponse struct {
	Allow   bool   `json:"allow"`
	Subject string `json:"subject"`
}

func (a *httpAuthorizer) Authorize(ctx context.Context, slug, cookie string) (bool, string, error) {
	payload, err := json.Marshal(authzRequest{Slug: slug, Cookie: cookie})
	if err != nil {
		return false, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.authzURL, bytes.NewReader(payload))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("authz: unexpected status %d", resp.StatusCode)
	}
	var ar authzResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return false, "", err
	}
	return ar.Allow, ar.Subject, nil
}

// DeriveAuthzURL turns a routes endpoint URL into its sibling authz URL
// (routesURL/../authz), the DoR's generic convention. e.g.
// https://cp.example.com/ingress/routes -> https://cp.example.com/ingress/authz
func DeriveAuthzURL(routesURL string) (string, error) {
	u, err := url.Parse(routesURL)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimRight(u.Path, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		u.Path = "/authz"
	} else {
		u.Path = trimmed[:idx] + "/authz"
	}
	return u.String(), nil
}
