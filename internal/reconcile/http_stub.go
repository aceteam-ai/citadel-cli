package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// httpProvider is a THIN STUB implementation of DesiredStateProvider over HTTP.
//
// IT IS NOT WIRED INTO ANYTHING LIVE. The control-plane endpoints it talks to
// DO NOT EXIST YET — they are owned by aceteam-ai/aceteam#4273. The routes,
// auth (node device identity), and error/retry semantics below are PLACEHOLDERS
// that must be finalized against that issue before this is used. It exists only
// to (a) document the intended transport shape next to the wire-contract types
// and (b) give the later live-provider increment a starting point. Do not
// construct it from production code paths.
type httpProvider struct {
	// BaseURL is the control-plane base, e.g. "https://aceteam.ai/fabric".
	BaseURL string
	// NodeID identifies this node (device identity).
	NodeID string
	// Client is the HTTP client; nil uses http.DefaultClient.
	Client *http.Client
	// Authorize, if set, stamps auth headers (device-identity token) onto each
	// request. The real device-identity auth is TBD per aceteam#4273.
	Authorize func(*http.Request)
}

func (p *httpProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// Fetch GETs <BaseURL>/nodes/{id}/desired-state. ENDPOINT TBD per aceteam#4273.
func (p *httpProvider) Fetch(ctx context.Context) (DesiredState, error) {
	url := fmt.Sprintf("%s/nodes/%s/desired-state", p.BaseURL, p.NodeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DesiredState{}, err
	}
	if p.Authorize != nil {
		p.Authorize(req)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return DesiredState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DesiredState{}, fmt.Errorf("desired-state fetch: unexpected status %d", resp.StatusCode)
	}
	var ds DesiredState
	if err := json.NewDecoder(resp.Body).Decode(&ds); err != nil {
		return DesiredState{}, fmt.Errorf("decode desired-state: %w", err)
	}
	return ds, nil
}

// Report POSTs <BaseURL>/nodes/{id}/actual-state. ENDPOINT TBD per aceteam#4273.
func (p *httpProvider) Report(ctx context.Context, actual ActualState) error {
	url := fmt.Sprintf("%s/nodes/%s/actual-state", p.BaseURL, p.NodeID)
	body, err := json.Marshal(actual)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Authorize != nil {
		p.Authorize(req)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("actual-state report: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// compile-time check that the stub satisfies the interface.
var _ DesiredStateProvider = (*httpProvider)(nil)
