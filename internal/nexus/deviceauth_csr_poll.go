package nexus

import (
	"errors"
	"sync"
)

// NewCSRTokenPoller binds one grant's CSR and fallback state to its poll callback.
// It retains no returned token. Certificate-only refusal falls back on the next
// poll; access denial, expiry and unrelated errors retain their ordinary meaning.
func NewCSRTokenPoller(deviceCode, csrPEM string, check func(string, string) (*TokenResponse, error)) func(string) (*TokenResponse, error) {
	var mu sync.Mutex
	return func(code string) (*TokenResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if check == nil || deviceCode == "" || code != deviceCode {
			return nil, errors.New("device authorization grant mismatch")
		}
		token, err := check(deviceCode, csrPEM)
		var tokenErr *TokenError
		if csrPEM != "" && errors.As(err, &tokenErr) && isCertificateEnrollmentCode(tokenErr.ErrorCode) {
			csrPEM = ""
			return nil, &TokenError{ErrorCode: "authorization_pending"}
		}
		return token, err
	}
}
