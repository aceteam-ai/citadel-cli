// Device enrollment via the platform's interactive pairing flow (#5959).
//
// The pairing flow is the same trust-root event Citadel OS nodes use, with
// profile="device": the device generates a code + CSR, the operator approves
// in a signed-in browser (aceteam.ai/fabric/pair?code=...), and the platform
// returns an org authkey PLUS a long-TTL fabric CA leaf bound to the device's
// key. The leaf is what makes everything afterwards derived state.
package devicemode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

const (
	// pairingPollInterval matches the backend's documented 2s node poll.
	pairingPollInterval = 2 * time.Second
	// pairingSessionTTL mirrors the backend's 5-minute session TTL; polling
	// stops shortly after it since the session cannot confirm anymore.
	pairingSessionTTL = 5 * time.Minute

	codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I ambiguity
	codeLength   = 6
)

// devicePairingStartRequest is the pairing-start body with the device profile.
// It extends the node request shape (nodeidentity.PairingStartRequest) with
// the profile field the backend uses to select the device leaf TTL.
type devicePairingStartRequest struct {
	Code      string                `json:"code"`
	NodeInfo  nodeidentity.NodeInfo `json:"node_info"`
	CSRPem    string                `json:"csr_pem,omitempty"`
	MachineID string                `json:"machine_id,omitempty"`
	Profile   string                `json:"profile"`
}

// EnrollResult is what a confirmed pairing session yields.
type EnrollResult struct {
	Authkey  string
	NodeUID  string
	LeafPem  string
	ChainPem string
}

// NewPairingCode generates a random 6-char pairing code from an unambiguous
// alphabet.
func NewPairingCode() (string, error) {
	buf := make([]byte, codeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}
	code := make([]byte, codeLength)
	for i, b := range buf {
		code[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(code), nil
}

// PairURL returns the browser approval URL the operator opens.
func PairURL(apiBaseURL, code string) string {
	return fmt.Sprintf("%s/fabric/pair?code=%s", apiBaseURL, code)
}

// StartDevicePairing opens a pairing session carrying the device CSR and
// profile. The CSR is generated from the identity store's key (created on
// first use), so the private key never leaves the machine.
func StartDevicePairing(
	ctx context.Context,
	client *http.Client,
	apiBaseURL, code string,
	store *nodeidentity.Store,
	hostname, machineID string,
) error {
	key, err := store.GetOrCreateKey()
	if err != nil {
		return fmt.Errorf("device identity key: %w", err)
	}
	csrPEM, err := store.GenerateCSR(key)
	if err != nil {
		return fmt.Errorf("generate CSR: %w", err)
	}

	body := devicePairingStartRequest{
		Code:      code,
		NodeInfo:  nodeidentity.NodeInfo{Hostname: hostname},
		CSRPem:    string(csrPEM),
		MachineID: machineID,
		Profile:   "device",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal pairing request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, apiBaseURL+"/api/fabric/pairing/start", bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("build pairing request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("start pairing session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pairing start returned HTTP %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// WaitForApproval polls the pairing status endpoint until the operator
// approves in the browser, the session expires, or ctx is canceled.
func WaitForApproval(
	ctx context.Context,
	client *http.Client,
	apiBaseURL, code string,
) (*EnrollResult, error) {
	deadline := time.Now().Add(pairingSessionTTL + 30*time.Second)
	ticker := time.NewTicker(pairingPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pairing session expired without approval")
		}

		status, err := pollStatus(ctx, client, apiBaseURL, code)
		if err != nil {
			// Transient network errors should not abort an interactive wait.
			continue
		}
		switch status.Status {
		case "confirmed":
			if status.Authkey == "" {
				return nil, fmt.Errorf("pairing confirmed but no authkey returned")
			}
			return &EnrollResult{
				Authkey:  status.Authkey,
				NodeUID:  status.NodeUID,
				LeafPem:  status.LeafPem,
				ChainPem: status.ChainPem,
			}, nil
		case "expired":
			return nil, fmt.Errorf("pairing session expired without approval")
		}
	}
}

func pollStatus(
	ctx context.Context,
	client *http.Client,
	apiBaseURL, code string,
) (*nodeidentity.PairingStatusResponse, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		fmt.Sprintf("%s/api/fabric/pairing/%s/status", apiBaseURL, code), nil,
	)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pairing status returned HTTP %d", resp.StatusCode)
	}
	var status nodeidentity.PairingStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("parse pairing status: %w", err)
	}
	return &status, nil
}
