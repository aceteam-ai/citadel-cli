package declaredsession

import (
	"context"
	"errors"
)

// Peer diagnostics never become arbitrary error text. Only this finite set of
// holder-authored classifications may cross the attachment boundary.
func attachmentDisconnectReason(reason string) string {
	switch reason {
	case "slow_viewer", "protocol_error", "controller_required", "busy", "holder_stopped":
		return reason
	default:
		return "protocol_error"
	}
}

type attachmentDisconnectError struct{ reason string }

func (e *attachmentDisconnectError) Error() string {
	return "declared session holder disconnected: " + attachmentDisconnectReason(e.reason)
}

// AttachmentDisconnectReason exposes only a reviewed static classification.
// Unrelated errors, including errors with similar text, carry no peer reason.
func AttachmentDisconnectReason(err error) string {
	var disconnected *attachmentDisconnectError
	if errors.As(err, &disconnected) && disconnected != nil {
		return attachmentDisconnectReason(disconnected.reason)
	}
	return ""
}

func disconnectFrame(reason string) wireFrame {
	f := frame("error")
	f.Reason = attachmentDisconnectReason(reason)
	return f
}

func sendAttachmentDisconnect(ctx context.Context, f *framedConn, reason string) {
	// The actor only cancels/enqueues a reason. This bounded final write runs in
	// the connection worker after the in-flight writer has stopped, never under
	// the holder's fanout/state lock. Failure is still a non-success EOF to the
	// client, not evidence of graceful cleanup or log durability.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationWait)
	defer cancel()
	_ = f.write(writeCtx, disconnectFrame(reason))
}
