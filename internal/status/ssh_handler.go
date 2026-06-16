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

// vpnCIDR is the Headscale CGNAT range. Requests from this range are trusted
// because only nodes on the Headscale VPN mesh can originate from it.
const vpnCIDR = "100.64.0.0/10"

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

// requireVPNOrAuth allows the request if it comes from the VPN mesh OR if
// a valid terminal token is presented. SSH key deployment from the platform
// relay will use VPN-origin auth (the relay runs on the VPN). Direct API
// callers can use token-based auth.
func (s *Server) requireVPNOrAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Accept VPN-origin requests without a token
		if isVPNOrigin(r) {
			next(w, r)
			return
		}

		// Fall back to token-based auth if available
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
		http.Error(w, `{"error":"authorization required: VPN origin or valid token"}`, http.StatusUnauthorized)
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
