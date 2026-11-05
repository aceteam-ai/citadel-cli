package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const CurrentDeviceBindingPath = "/api/fabric/device-auth/current-binding"
const CurrentDeviceBindingTimeout = 5 * time.Second
const maxCurrentBindingBytes = 4096

// DeviceBindingState is display evidence for one request, never authority.
type DeviceBindingState uint8

const (
	BindingUnknown DeviceBindingState = iota
	BindingConfirmed
	BindingNotLinked
	BindingRefused
	BindingUnsupported
)

// DeviceBindingRequest captures the current credential and independent live
// mesh identity. Callers must recheck them before consuming a confirmation.
// Do not log this value: it contains the bearer.
type DeviceBindingRequest struct {
	APIBaseURL string
	Bearer     string
	OrgID      string
	NodeID     string
}

func canonicalBindingNodeID(id string) bool {
	if len(id) == 0 || len(id) > 128 || id[0] < '1' || id[0] > '9' {
		return false
	}
	for _, b := range []byte(id) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func canonicalBindingOrgID(id string) bool {
	value, err := uuid.Parse(id)
	return err == nil && value != uuid.Nil && value.String() == id
}

// Valid rejects ambiguous origins and missing comparison inputs before I/O.
func (r DeviceBindingRequest) Valid() bool {
	u, err := url.Parse(r.APIBaseURL)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") &&
		u.Hostname() != "" && u.User == nil && u.Opaque == "" &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(r.APIBaseURL, "#") &&
		r.Bearer != "" && len(r.Bearer) <= 4096 && strings.TrimSpace(r.Bearer) == r.Bearer &&
		!strings.ContainsAny(r.Bearer, "\r\n") &&
		canonicalBindingOrgID(r.OrgID) && canonicalBindingNodeID(r.NodeID)
}

// LookupCurrentDeviceBinding makes one uncached, non-redirecting observation.
// A private fresh HTTP/1 transport avoids reused-connection request replay and
// response-body inspection by the platform transport. Refusal bodies are closed
// unread. Neither raw errors nor response identities escape into diagnostics.
func LookupCurrentDeviceBinding(ctx context.Context, input DeviceBindingRequest) DeviceBindingState {
	if !input.Valid() {
		return BindingUnknown
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: CurrentDeviceBindingTimeout}).DialContext,
		TLSHandshakeTimeout:   CurrentDeviceBindingTimeout,
		ResponseHeaderTimeout: CurrentDeviceBindingTimeout,
		DisableKeepAlives:     true,
		Protocols:             protocols,
	}
	defer transport.CloseIdleConnections()
	return lookupCurrentDeviceBinding(ctx, input, &http.Client{Transport: transport})
}

func lookupCurrentDeviceBinding(ctx context.Context, input DeviceBindingRequest, client *http.Client) DeviceBindingState {
	if !input.Valid() || client == nil {
		return BindingUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, CurrentDeviceBindingTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return BindingUnknown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(input.APIBaseURL, "/")+CurrentDeviceBindingPath, nil)
	if err != nil {
		return BindingUnknown
	}
	req.Header.Set("Authorization", "Bearer "+input.Bearer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	once := *client
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	once.Jar = nil
	if once.Timeout <= 0 || once.Timeout > CurrentDeviceBindingTimeout {
		once.Timeout = CurrentDeviceBindingTimeout
	}
	response, err := once.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return BindingUnknown
	}
	defer response.Body.Close()
	if ctx.Err() != nil {
		return BindingUnknown
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return BindingRefused
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return BindingUnsupported
	case http.StatusOK:
	default:
		return BindingUnknown
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return BindingUnknown
	}
	noStore := false
	for _, directive := range strings.Split(response.Header.Get("Cache-Control"), ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
			noStore = true
		}
	}
	if !noStore {
		return BindingUnknown
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCurrentBindingBytes+1))
	if err != nil || len(body) > maxCurrentBindingBytes || ctx.Err() != nil {
		return BindingUnknown
	}
	state := decodeCurrentBinding(body, input)
	if ctx.Err() != nil {
		return BindingUnknown
	}
	return state
}

// bindingObject requires exact case-sensitive fields and rejects duplicates,
// nonobjects and trailing JSON. Nested node fields are checked independently.
func bindingObject(body []byte, allowed ...string) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		valid := false
		for _, candidate := range allowed {
			valid = valid || name == candidate
		}
		if _, duplicate := fields[name]; !valid || duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return fields, true
}

func decodeCurrentBinding(body []byte, expected DeviceBindingRequest) DeviceBindingState {
	fields, ok := bindingObject(body, "state", "org_id", "node", "observed_at")
	var state, observed string
	if !ok || json.Unmarshal(fields["state"], &state) != nil || json.Unmarshal(fields["observed_at"], &observed) != nil {
		return BindingUnknown
	}
	// Informational UTC milliseconds only; never compare to the host wall
	// clock, or turn this server timestamp into a cached freshness interval.
	const utcMillis = "2006-01-02T15:04:05.000Z"
	at, err := time.Parse(utcMillis, observed)
	if err != nil || len(observed) != 24 || at.Format(utcMillis) != observed {
		return BindingUnknown
	}
	if state == "not_linked" && len(fields) == 2 {
		return BindingNotLinked
	}
	if state != "confirmed" || len(fields) != 4 {
		return BindingUnknown
	}
	var org, domain, nodeID string
	node, ok := bindingObject(fields["node"], "domain", "id")
	if !ok || len(node) != 2 || json.Unmarshal(fields["org_id"], &org) != nil ||
		json.Unmarshal(node["domain"], &domain) != nil || json.Unmarshal(node["id"], &nodeID) != nil {
		return BindingUnknown
	}
	if !canonicalBindingOrgID(org) || !canonicalBindingNodeID(nodeID) ||
		org != expected.OrgID || domain != "headscale" || nodeID != expected.NodeID {
		return BindingUnknown
	}
	return BindingConfirmed
}
