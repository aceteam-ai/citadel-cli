package nodeidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PairingStartRequest is the additive-cert-aware body posted to
// POST /api/fabric/pairing/start. CSRPem and MachineID are optional and, when
// present, ask the backend to issue a fabric CA leaf cert alongside the authkey
// (P1 of #4583). When omitted the request is byte-for-byte the authkey-only
// request the backend has always accepted, so this is fully back-compatible.
type PairingStartRequest struct {
	Code      string   `json:"code"`
	NodeInfo  NodeInfo `json:"node_info"`
	CSRPem    string   `json:"csr_pem,omitempty"`
	MachineID string   `json:"machine_id,omitempty"`
}

// NodeInfo mirrors the backend's optional hardware/identity metadata.
type NodeInfo struct {
	Hostname string `json:"hostname,omitempty"`
	GPU      string `json:"gpu,omitempty"`
	IP       string `json:"ip,omitempty"`
}

// PairingStatusResponse is the polled response from
// GET /api/fabric/pairing/{code}/status. LeafPem/ChainPem/NodeUID are populated
// only when the node sent a CSR at start AND the backend fabric CA is
// activated; all three are nil/empty in authkey-only mode.
type PairingStatusResponse struct {
	Status   string `json:"status"`
	Authkey  string `json:"authkey,omitempty"`
	LeafPem  string `json:"leaf_pem,omitempty"`
	ChainPem string `json:"chain_pem,omitempty"`
	NodeUID  string `json:"node_uid,omitempty"`
}

// BuildPairingStartRequest assembles a pairing-start body that carries the
// node's CSR and machine_id. The CSR is generated from the store's key
// (creating the key on first use). Callers that don't want a cert can build the
// request without this helper; this helper always attaches the cert fields.
//
// It never returns the private key and never logs it.
func (s *Store) BuildPairingStartRequest(code string, node NodeInfo, machineID string) (*PairingStartRequest, error) {
	key, err := s.GetOrCreateKey()
	if err != nil {
		return nil, fmt.Errorf("node identity key: %w", err)
	}
	csrPEM, err := s.GenerateCSR(key)
	if err != nil {
		return nil, fmt.Errorf("generate CSR: %w", err)
	}
	return &PairingStartRequest{
		Code:      code,
		NodeInfo:  node,
		CSRPem:    string(csrPEM),
		MachineID: machineID,
	}, nil
}

// StoreLeafFromStatus persists any leaf + chain the pairing status response
// carried. It is the "results flow up" half of pairing: back-compat safe (no-op
// when the backend returned no cert) and best-effort (a store failure is
// returned but the caller should treat it as non-fatal — pairing succeeds on
// the authkey alone).
func (s *Store) StoreLeafFromStatus(resp *PairingStatusResponse) (stored bool, err error) {
	if resp == nil || (resp.LeafPem == "" && resp.ChainPem == "") {
		return false, nil
	}
	if err := s.StoreLeaf(resp.LeafPem, resp.ChainPem); err != nil {
		return false, err
	}
	return true, nil
}

// FetchCAChain GETs the public fabric CA trust chain from
// {baseURL}/api/fabric/ca/chain and caches it in the store. The chain is public
// material and requires no auth.
//
// Degrades gracefully: if the CA is not activated the endpoint returns 503, in
// which case FetchCAChain returns ErrCANotActivated and stores nothing. Callers
// on the pairing/init path must treat any error here as non-fatal.
func (s *Store) FetchCAChain(ctx context.Context, baseURL string, client *http.Client) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	url := baseURL + "/api/fabric/ca/chain"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build CA chain request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch CA chain: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrCANotActivated
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch CA chain: unexpected status %d", resp.StatusCode)
	}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("read CA chain body: %w", err)
	}
	return s.StoreCAChain(buf.String())
}

// ErrCANotActivated indicates the backend fabric CA is not yet configured
// (HTTP 503 from /api/fabric/ca/chain). Callers should degrade gracefully.
var ErrCANotActivated = fmt.Errorf("fabric CA not activated")

// jsonMarshal is a tiny indirection so tests can confirm the wire shape.
func (r *PairingStartRequest) jsonMarshal() ([]byte, error) {
	return json.Marshal(r)
}
