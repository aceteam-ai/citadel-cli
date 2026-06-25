package catalog

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ProbeResult classifies the outcome of a best-effort module health probe used
// by `module update` to decide whether a freshly re-installed+restarted module is
// healthy enough to keep, or should be rolled back.
type ProbeResult int

const (
	// ProbeNotProbeable: there is nothing usable to probe (no health_check, or no
	// endpoint/port), OR the target is not yet listening. We DO NOT roll back on
	// this -- the install may simply not start a long-running server. Best-effort.
	ProbeNotProbeable ProbeResult = iota
	// ProbeHealthy: the probe got a definitive healthy response.
	ProbeHealthy
	// ProbeUnhealthy: the probe got a definitive bad response (e.g. HTTP 5xx).
	// This is the ONLY result that triggers a rollback.
	ProbeUnhealthy
)

func (r ProbeResult) String() string {
	switch r {
	case ProbeHealthy:
		return "healthy"
	case ProbeUnhealthy:
		return "unhealthy"
	default:
		return "not-probeable"
	}
}

// HasHealthProbe reports whether a HealthCheck declares enough to probe at all.
// Pure -- table-tested.
func HasHealthProbe(hc HealthCheck) bool {
	return hc.Port > 0 || strings.TrimSpace(hc.Endpoint) != ""
}

// healthProbeTimeout bounds a single probe. Kept small: the probe is a
// best-effort gate, not a readiness wait loop.
func healthProbeTimeout(hc HealthCheck) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(hc.Timeout)); err == nil && d > 0 {
		return d
	}
	return 3 * time.Second
}

// ProbeHealth runs a single best-effort health probe against a service on the
// local host, given its declared HealthCheck. Semantics (deliberately
// conservative so we never roll back spuriously):
//
//   - no port and no endpoint -> ProbeNotProbeable.
//   - HTTP endpoint declared: GET http://127.0.0.1:<port><endpoint>. A 2xx/3xx/4xx
//     means the server is up and answering -> ProbeHealthy. A 5xx -> ProbeUnhealthy.
//     A connection error (refused / timeout / DNS) -> ProbeNotProbeable (the
//     service may not run a server, or isn't up yet -- not a rollback trigger).
//   - port only (no endpoint): a TCP connect that succeeds -> ProbeHealthy; a
//     failure -> ProbeNotProbeable.
//
// The host is always loopback: a module runs its container on this node, and the
// probe is a sanity check, not a remote health monitor.
func ProbeHealth(hc HealthCheck) ProbeResult {
	if !HasHealthProbe(hc) {
		return ProbeNotProbeable
	}
	timeout := healthProbeTimeout(hc)

	endpoint := strings.TrimSpace(hc.Endpoint)
	if endpoint != "" && hc.Port > 0 {
		path := endpoint
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		url := fmt.Sprintf("http://127.0.0.1:%d%s", hc.Port, path)
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(url)
		if err != nil {
			// Not up / not an HTTP server / not yet listening: not a rollback trigger.
			return ProbeNotProbeable
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return ProbeUnhealthy
		}
		return ProbeHealthy
	}

	if hc.Port > 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", hc.Port)
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return ProbeNotProbeable
		}
		_ = conn.Close()
		return ProbeHealthy
	}

	return ProbeNotProbeable
}
