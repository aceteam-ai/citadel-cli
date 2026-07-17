// internal/status/ssh_handler.go
// HTTP handler for receiving SSH public keys from the platform and writing
// them to ~/.ssh/authorized_keys. This endpoint is called by the Python
// relay after org-ownership validation.
package status

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// sshAuthorizedKeysRequest is the JSON body for POST /ssh/authorized-keys.
type sshAuthorizedKeysRequest struct {
	Keys []string `json:"keys"`
}

// sshAuthorizedKeysResponse is the JSON response from POST /ssh/authorized-keys.
type sshAuthorizedKeysResponse struct {
	Added   int `json:"added"`
	Skipped int `json:"skipped"`
	Total   int `json:"total"`
}

// vpnCIDR is the Headscale CGNAT range. Source IP in this range is a coarse
// network signal, not a caller identity, so mesh-origin trust is being retired
// in favor of a per-org token behind RequireControlTokenEnvVar (issue #5028).
const vpnCIDR = "100.64.0.0/10"

// RequireControlTokenEnvVar gates the interim :8080 control-token enforcement
// (issue #5028 lever B). It is OFF unless explicitly set to a truthy value
// ("1", "true", "yes", "on"). When on, requireVPNOrAuth drops the VPN-origin
// bypass and demands the per-org terminal token even from mesh peers. Kept OFF
// by default so the change is a deliberate, reversible flip (a code-free
// rollback is just unsetting the env) rather than a hard break for older relays
// that still dial the mesh without a bearer.
const RequireControlTokenEnvVar = "CITADEL_REQUIRE_CONTROL_TOKEN"

// RequireControlTokenEnabled reports whether the control-token flip is on,
// reading RequireControlTokenEnvVar with the same truthy set used elsewhere.
func RequireControlTokenEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireControlTokenEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// isVPNOrigin checks if the request originates from the Headscale VPN mesh.
func isVPNOrigin(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr might be just an IP without port
		host = r.RemoteAddr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	_, cidr, err := net.ParseCIDR(vpnCIDR)
	if err != nil {
		return false
	}

	return cidr.Contains(ip)
}

// requireVPNOrAuth gates the control endpoints (/agent/*, provisioning, workflow)
// on the plaintext status listener.
//
// Historically it accepted a request whose source IP is in the Headscale CGNAT
// range without a token, treating mesh origin as sufficient trust. Source IP is
// a coarse network signal rather than a caller identity (issue #5028), so this
// tightens the check to require the per-org terminal token (the same
// server-decryptable token already validated on the :7860 terminal listener)
// for mesh origins too.
//
// The mesh-origin bypass is dropped only when requireControlToken is set (the
// deliberate flip, wired from CITADEL_REQUIRE_CONTROL_TOKEN in cmd/work.go).
// Default keeps the legacy VPN-origin trust so older relays that dial over the
// mesh without presenting a token do not break at merge; the flip is thrown in a
// planned window after the relay is updated to send the bearer. See issue #5028
// section 8 and mtls.go for the durable per-caller identity (#5959) that
// supersedes this interim bearer.
func (s *Server) requireVPNOrAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Accept VPN-origin requests without a token UNLESS the control-token flip
		// is on, in which case mesh origin no longer auto-trusts (#5028 lever B).
		if !s.requireControlToken && isVPNOrigin(r) {
			next(w, r)
			return
		}

		// Require a valid per-org terminal token. This is the only accepted path
		// once requireControlToken is set, and the fallback for non-mesh callers
		// otherwise.
		if s.tokenValidator != nil {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != "" && token != r.Header.Get("Authorization") {
				if _, err := s.tokenValidator.ValidateToken(token, s.orgID); err == nil {
					next(w, r)
					return
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"authorization required: valid org token"}`, http.StatusUnauthorized)
	}
}

// handleSSHAuthorizedKeys handles POST /ssh/authorized-keys.
// It accepts a list of SSH public keys, validates their format, deduplicates
// against existing keys, and appends new ones to ~/.ssh/authorized_keys.
func (s *Server) handleSSHAuthorizedKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read and parse request body (limit to 1MB)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
	if err != nil {
		writeJSONError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var req sshAuthorizedKeysRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if len(req.Keys) == 0 {
		writeJSONError(w, "no keys provided", http.StatusBadRequest)
		return
	}

	// Validate all keys before writing any
	for i, key := range req.Keys {
		if err := validateSSHPublicKey(key); err != nil {
			writeJSONError(w, fmt.Sprintf("invalid key at index %d: %s", i, err), http.StatusBadRequest)
			return
		}
	}

	// Perform the write
	result, err := deploySSHKeys(req.Keys)
	if err != nil {
		writeJSONError(w, fmt.Sprintf("failed to deploy keys: %s", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// validateSSHPublicKey checks that the string is a valid SSH public key.
func validateSSHPublicKey(key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return fmt.Errorf("empty key")
	}

	// Use golang.org/x/crypto/ssh to parse and validate the key.
	// ParseAuthorizedKey handles all standard key types (ssh-rsa, ssh-ed25519,
	// ecdsa-sha2-nistp256, ecdsa-sha2-nistp384, ecdsa-sha2-nistp521, sk-* keys).
	_, _, _, _, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return fmt.Errorf("not a valid SSH public key: %w", err)
	}

	return nil
}

// keyMaterial extracts the type and base64 body (fields 1-2) from an SSH key
// line, normalizing the comparison so comments don't affect deduplication.
func keyMaterial(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) >= 2 {
		return fields[0] + " " + fields[1]
	}
	return strings.TrimSpace(line)
}

// deploySSHKeys appends new SSH public keys to ~/.ssh/authorized_keys,
// deduplicating against existing keys. Returns counts of added, skipped,
// and total keys in the file after the operation.
func deploySSHKeys(keys []string) (*sshAuthorizedKeysResponse, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	sshDir := filepath.Join(homeDir, ".ssh")
	authKeysPath := filepath.Join(sshDir, "authorized_keys")

	// Ensure ~/.ssh exists with correct permissions
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	// Read existing keys and build a set of key material for dedup
	existingSet := make(map[string]bool)
	var existingLines []string

	if file, err := os.Open(authKeysPath); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			existingLines = append(existingLines, line)
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				existingSet[keyMaterial(trimmed)] = true
			}
		}
		file.Close()
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("failed to read authorized_keys: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to open authorized_keys: %w", err)
	}

	// Determine which keys to add
	added := 0
	skipped := 0
	var newKeys []string

	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		km := keyMaterial(trimmed)
		if existingSet[km] {
			skipped++
		} else {
			newKeys = append(newKeys, trimmed)
			existingSet[km] = true
			added++
		}
	}

	// If there are new keys, append them atomically
	if added > 0 {
		// Build the full file content: existing lines + new keys
		var content strings.Builder
		for _, line := range existingLines {
			content.WriteString(line)
			content.WriteString("\n")
		}
		// Add a blank line separator if the file didn't end with one
		if len(existingLines) > 0 {
			lastLine := existingLines[len(existingLines)-1]
			if strings.TrimSpace(lastLine) != "" {
				content.WriteString("\n")
			}
		}
		for _, key := range newKeys {
			content.WriteString(key)
			content.WriteString("\n")
		}

		// Write atomically via temp file + rename
		tmpPath := authKeysPath + ".tmp"
		if err := os.WriteFile(tmpPath, []byte(content.String()), 0600); err != nil {
			return nil, fmt.Errorf("failed to write temporary file: %w", err)
		}
		if err := os.Rename(tmpPath, authKeysPath); err != nil {
			os.Remove(tmpPath)
			return nil, fmt.Errorf("failed to update authorized_keys: %w", err)
		}
	}

	return &sshAuthorizedKeysResponse{
		Added:   added,
		Skipped: skipped,
		Total:   len(existingSet),
	}, nil
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
