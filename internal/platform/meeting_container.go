// internal/platform/meeting_container.go
//
// CDPBrowser: drive a Chromium that ANOTHER process launched, over CDP on a
// host-PUBLISHED debug port (aceteam-ai/citadel-cli#514). This is the container
// half of the meeting media stack — the meeting module's in-container supervisor
// (meetingd) launches and reaps Chrome on a managed Xvfb inside the container,
// and this type only speaks CDP to it. It owns no process, display, or profile,
// unlike its sibling MeetingBrowser, so the fragile Google Meet DOM logic in the
// MEETING_JOIN handler runs UNCHANGED against either backend (both satisfy the
// same browser surface).
//
// The load-bearing subtlety (why cdpCommand can't be used verbatim): a
// containerized Chrome binds its DevTools endpoint on the CONTAINER's loopback
// (e.g. 127.0.0.1:9222) and advertises exactly that in the webSocketDebuggerUrl
// it returns from /json. From the host, only the compose's PUBLISHED port
// (127.0.0.1:8208 -> container 9223 -> socat -> chrome 9222) is reachable; the
// advertised 9222 is not. So the /json fetch is done through the published port
// (works — it is forwarded), but the ws URL inside it must be rewritten to the
// published host:port before dialing. See cdpCommandPublished + rewriteLoopbackWSPort.
package platform

import (
	"fmt"
	"net/url"
	"time"
)

// rewriteLoopbackWSPort rewrites the host:port of a DevTools WebSocket URL to
// 127.0.0.1:port, preserving the scheme and the /devtools/page/<id> path. Pure,
// so the container CDP rewrite is unit-testable without a live browser.
func rewriteLoopbackWSPort(wsURL string, port int) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parse CDP websocket url %q: %w", wsURL, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("not a websocket url: %q", wsURL)
	}
	u.Host = fmt.Sprintf("127.0.0.1:%d", port)
	return u.String(), nil
}

// cdpCommandPublished is cdpCommand for a Chrome reached through a Docker port
// PUBLISH. It fetches /json through the published port (forwarded to the
// container's DevTools endpoint, so this works verbatim) and then rewrites the
// advertised, container-internal webSocketDebuggerUrl to the published host:port
// before dialing — the advertised URL names a port that only exists inside the
// container.
func cdpCommandPublished(debugPort int, method string, params map[string]any) (map[string]any, error) {
	target, err := pickTarget(debugPort)
	if err != nil {
		return nil, err
	}
	wsURL, err := rewriteLoopbackWSPort(target.WebSocketDebuggerURL, debugPort)
	if err != nil {
		return nil, err
	}
	return cdpDialAndSend(wsURL, method, params)
}

// cdpEvaluatePublished mirrors cdpEvaluate for the published-port path: it runs a
// JS expression and returns its by-value result, surfacing a JS throw as a Go
// error, but rewrites the container-internal ws URL first.
func cdpEvaluatePublished(debugPort int, expression string) (any, error) {
	return cdpEvalValue(cdpCommandPublished(debugPort, "Runtime.evaluate", runtimeEvalParams(expression)))
}

// CDPBrowser drives a Chromium launched by the containerized meeting module over
// CDP on a host-published debug port. It satisfies the same browser surface the
// MEETING_JOIN join flow drives (Navigate/CurrentURL/Evaluate/Type/Close) as
// *MeetingBrowser, so the join/interactive logic is identical across backends.
// All CDP goes through the *Published helpers, which rewrite the
// container-internal ws URL to the published host port.
type CDPBrowser struct {
	debugPort int
}

// NewCDPBrowser constructs a CDP driver for a Chromium reachable on the given
// host-published debug port (the meeting module publishes CDP on
// services.MeetingCDPHostPort). It does not connect; call Ready first.
func NewCDPBrowser(debugPort int) *CDPBrowser {
	return &CDPBrowser{debugPort: debugPort}
}

// Ready blocks until the CDP endpoint answers AND a trivial Evaluate round-trips
// through the WebSocket forward, so a broken port publish or socat forward fails
// HERE with a clear error instead of at the first Navigate mid-join. The /json
// readiness poll alone is not enough: it is forwarded fine even when the ws path
// (which the advertised URL misdirects to the container-internal port) is broken.
func (b *CDPBrowser) Ready(timeout time.Duration) error {
	if err := waitForCDPReady(b.debugPort, timeout); err != nil {
		return fmt.Errorf("meeting container CDP not ready on host port %d: %w", b.debugPort, err)
	}
	if _, err := cdpEvaluatePublished(b.debugPort, "1"); err != nil {
		return fmt.Errorf("meeting container CDP websocket unreachable on host port %d "+
			"(port publish or in-container socat forward broken): %w", b.debugPort, err)
	}
	return nil
}

// Navigate drives the browser to a URL over CDP.
func (b *CDPBrowser) Navigate(rawURL string) error {
	_, err := cdpCommandPublished(b.debugPort, "Page.navigate", map[string]any{"url": rawURL})
	return err
}

// CurrentURL returns the active page's URL. The /json listing is fetched through
// the published port (forwarded), so no ws rewrite is needed here.
func (b *CDPBrowser) CurrentURL() (string, error) {
	t, err := pickTarget(b.debugPort)
	if err != nil {
		return "", err
	}
	return t.URL, nil
}

// Evaluate runs a JS expression and returns its by-value result; a JS throw is a
// Go error (see cdpEvalValue).
func (b *CDPBrowser) Evaluate(expression string) (any, error) {
	return cdpEvaluatePublished(b.debugPort, expression)
}

// Type sets the value of the first element matching selector, erroring if none
// matches (same throwing typeJS the host browser uses).
func (b *CDPBrowser) Type(selector, text string) error {
	_, err := cdpEvaluatePublished(b.debugPort, typeJS(selector, text))
	return err
}

// Close is a no-op: meetingd owns the container browser's process lifecycle
// (DELETE /sessions tears it down). Present so CDPBrowser satisfies the same
// interface as MeetingBrowser.
func (b *CDPBrowser) Close() error { return nil }
