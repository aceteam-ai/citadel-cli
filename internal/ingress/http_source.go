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
	return &httpRoutesSource{
		baseURL: routesURL,
		token:   token,
		client:  clientWithoutRedirects(client, 15*time.Second),
	}
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
	return &httpAuthorizer{
		authzURL: authzURL,
		token:    token,
		client:   clientWithoutRedirects(client, 10*time.Second),
	}
}

// clientWithoutRedirects returns a private copy of client which exposes the
// first redirect response to the caller instead of forwarding control-plane
// credentials or request bodies to the redirect target. The copy is important:
// callers may share their client with unrelated traffic whose redirect policy
// must remain untouched.
func clientWithoutRedirects(client *http.Client, defaultTimeout time.Duration) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	cloned := *client
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &cloned
}

type authzRequest struct {
	Slug   string `json:"slug"`
	Cookie string `json:"cookie"`
}

type authzResponse struct {
	Allow   bool   `json:"allow"`
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

func (a *httpAuthorizer) Authorize(ctx context.Context, slug, cookie string) (AuthzDecision, error) {
	payload, err := json.Marshal(authzRequest{Slug: slug, Cookie: cookie})
	if err != nil {
		return AuthzDecision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.authzURL, bytes.NewReader(payload))
	if err != nil {
		return AuthzDecision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return AuthzDecision{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return AuthzDecision{}, fmt.Errorf("authz: unexpected status %d", resp.StatusCode)
	}
	var ar authzResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return AuthzDecision{}, err
	}
	return AuthzDecision{Allow: ar.Allow, Subject: ar.Subject, Reason: ar.Reason}, nil
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
