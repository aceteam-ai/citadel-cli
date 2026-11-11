package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

var enrollmentNodeIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// DeviceLinkFailureKind is a closed diagnostic vocabulary, not retry or
// revocation authority. Definite refusals still use DeviceLinkRefusedError.
type DeviceLinkFailureKind string

const (
	DeviceLinkTransport         DeviceLinkFailureKind = "transport"
	DeviceLinkHTTPServer        DeviceLinkFailureKind = "http_server"
	DeviceLinkHTTPStatus        DeviceLinkFailureKind = "http_status"
	DeviceLinkRefused           DeviceLinkFailureKind = "refused"
	DeviceLinkMalformedResponse DeviceLinkFailureKind = "malformed_response"
	DeviceLinkLocalPrecondition DeviceLinkFailureKind = "local_precondition"
	DeviceLinkUnconfirmed       DeviceLinkFailureKind = "unconfirmed"
)

// DeviceLinkFailure deliberately retains no underlying error, URL, grant, or
// response body. Both its constructor and diagnostic projection fail closed on
// unknown kinds or statuses outside the kind's documented range. The refused
// category is reserved for the existing DeviceLinkRefusedError type.
type DeviceLinkFailure struct {
	kind       DeviceLinkFailureKind
	statusCode int
}

func NewDeviceLinkFailure(kind DeviceLinkFailureKind, statusCode int) *DeviceLinkFailure {
	kind, statusCode = normalizedDeviceLinkDetails(kind, statusCode)
	return &DeviceLinkFailure{kind: kind, statusCode: statusCode}
}

func normalizedDeviceLinkDetails(kind DeviceLinkFailureKind, statusCode int) (DeviceLinkFailureKind, int) {
	valid := false
	switch kind {
	case DeviceLinkTransport, DeviceLinkLocalPrecondition, DeviceLinkUnconfirmed:
		valid = statusCode == 0
	case DeviceLinkHTTPServer:
		valid = statusCode >= 500 && statusCode <= 599
	case DeviceLinkHTTPStatus:
		valid = statusCode >= 100 && statusCode < 400 && statusCode != http.StatusOK
	case DeviceLinkMalformedResponse:
		valid = statusCode == http.StatusOK
	}
	if !valid {
		return DeviceLinkUnconfirmed, 0
	}
	return kind, statusCode
}

// DeviceLinkFailureDetails inspects only recognized error types. It never calls
// Error on an opaque cause or parses arbitrary text for diagnostic authority.
func DeviceLinkFailureDetails(err error) (DeviceLinkFailureKind, int) {
	var refused *DeviceLinkRefusedError
	if errors.As(err, &refused) && refused != nil {
		if refused.StatusCode == http.StatusOK || (refused.StatusCode >= 400 && refused.StatusCode < 500) {
			return DeviceLinkRefused, refused.StatusCode
		}
		return DeviceLinkUnconfirmed, 0
	}
	var failure *DeviceLinkFailure
	if errors.As(err, &failure) && failure != nil {
		return normalizedDeviceLinkDetails(failure.kind, failure.statusCode)
	}
	return DeviceLinkUnconfirmed, 0
}

// DeviceLinkDiagnostic renders only the closed kind and validated status.
func DeviceLinkDiagnostic(err error) string {
	kind, statusCode := DeviceLinkFailureDetails(err)
	if statusCode != 0 {
		return fmt.Sprintf("category=%s, HTTP %d", kind, statusCode)
	}
	return "category=" + string(kind)
}

func (e *DeviceLinkFailure) Error() string {
	return "device link failed (" + DeviceLinkDiagnostic(e) + ")"
}

// DeviceLinkRefusedError is a definite answer from the link route: a 4xx, or a
// 200 that does not confirm the link. Transport failures, timeouts and 5xx are
// not refusals; they say nothing about whether this enrollment can be linked.
type DeviceLinkRefusedError struct {
	StatusCode int
}

func (e *DeviceLinkRefusedError) Error() string {
	return "device link failed (" + DeviceLinkDiagnostic(e) + "); enrollment is incomplete"
}

// LinkRegisteredNode completes the current device grant after registration.
// The server verifies the used registration key against its reservation; neither
// a hostname nor an IP address is an enrollment proof. The request must never
// expose its grant in diagnostics or forward it on redirects. The public link
// route authorizes the grant plus reserved registration proof, not a bearer.
func (c *DeviceAuthClient) LinkRegisteredNode(ctx context.Context, deviceCode, nodeID string) error {
	if deviceCode == "" || !enrollmentNodeIDPattern.MatchString(nodeID) {
		return NewDeviceLinkFailure(DeviceLinkLocalPrecondition, 0)
	}
	body, _ := json.Marshal(struct {
		NodeID string `json:"nodeId"`
	}{nodeID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/fabric/device-config/"+url.PathEscape(deviceCode)+"/link", bytes.NewReader(body))
	if err != nil {
		return NewDeviceLinkFailure(DeviceLinkLocalPrecondition, 0)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors can contain the URL (and therefore the grant).
		return NewDeviceLinkFailure(DeviceLinkTransport, 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &DeviceLinkRefusedError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		kind := DeviceLinkHTTPStatus
		if resp.StatusCode >= 500 {
			kind = DeviceLinkHTTPServer
		}
		return NewDeviceLinkFailure(kind, resp.StatusCode)
	}
	success, err := readDeviceLinkConfirmation(resp.Body)
	if err != nil {
		return err
	}
	if !*success {
		return &DeviceLinkRefusedError{StatusCode: http.StatusOK}
	}
	return nil
}

// Keep the existing bounded, single-JSON response contract without formatting
// an arbitrary body-reader error, which may itself contain a grant-bearing URL.
func readDeviceLinkConfirmation(body io.Reader) (*bool, error) {
	malformed := func() (*bool, error) {
		return nil, NewDeviceLinkFailure(DeviceLinkMalformedResponse, http.StatusOK)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxDeviceAuthResponseBytes+1))
	if err != nil || len(data) > maxDeviceAuthResponseBytes {
		return malformed()
	}
	var result struct {
		Success *bool `json:"success"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&result) != nil || result.Success == nil {
		return malformed()
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return malformed()
	}
	return result.Success, nil
}

// ServerVersion reads the auth service's release version from /api/version.
// Callers use it only to interpret a link refusal; an unreadable version is
// reported as an error, never guessed.
func (c *DeviceAuthClient) ServerVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/version", nil)
	if err != nil {
		return "", fmt.Errorf("could not prepare server version request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("server version request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server version unavailable (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		Version string `json:"version"`
	}
	if err := decodeBoundedDeviceAuthJSON(resp.Body, &result, "server version response"); err != nil {
		return "", err
	}
	if result.Version == "" {
		return "", fmt.Errorf("server version response has no version")
	}
	return result.Version, nil
}
