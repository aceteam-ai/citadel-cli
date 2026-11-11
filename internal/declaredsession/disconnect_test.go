package declaredsession

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAttachmentDisconnectReasonsAreStaticAndNotSuccess(t *testing.T) {
	secret := "credential=never-render-this"
	for _, reason := range []string{"slow_viewer", "protocol_error", "controller_required", "busy", "holder_stopped", secret, ""} {
		connection := &memoryConnection{input: bytes.NewReader(nil)}
		f := &framedConn{conn: connection}
		message := frame("error")
		message.Reason = reason // Includes a malicious peer's arbitrary text.
		if err := f.write(context.Background(), message); err != nil {
			t.Fatal(err)
		}
		connection.input = bytes.NewReader(append([]byte(nil), connection.output.Bytes()...))
		attachment := &Attachment{client: &Client{framed: f}}
		if event, err := attachment.Next(context.Background()); err == nil || event.Kind != "" ||
			AttachmentDisconnectReason(err) != attachmentDisconnectReason(reason) || strings.Contains(err.Error(), secret) {
			t.Fatalf("disconnect reason=%q event=%#v err=%v", reason, event, err)
		}
	}
	if reason := AttachmentDisconnectReason(errors.New("slow_viewer: " + secret)); reason != "" {
		t.Fatal("untyped arbitrary diagnostic became a holder classification")
	}
}

func TestRejectedAttachmentSendsReasonWithoutStartingWriter(t *testing.T) {
	h, _ := holderFixture()
	server := &holderServers{holder: h, ctx: context.Background()}
	connection := &memoryConnection{input: bytes.NewReader(nil)}
	f := &framedConn{conn: connection}
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		request := <-h.requests
		request.reply <- holderReply{err: ErrRole}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.serveAttachment(ctx, cancel, f, frame("attach"))
	<-answered
	connection.input = bytes.NewReader(connection.output.Bytes())
	got, err := f.read(context.Background(), operationWait)
	if err != nil || got.Op != "error" || got.Reason != "controller_required" {
		t.Fatal("rejected attach did not publish its static refusal", got, err)
	}
}

func TestSlowViewerReasonIsPublishedOutsideActor(t *testing.T) {
	h, _ := holderFixture()
	viewer := testViewer("slow", true)
	for i := 0; i < cap(viewer.queue); i++ {
		viewer.queue <- frame("pong")
	}
	var reason string
	viewer.disconnect = func(observed string) { reason = observed }
	h.viewers[viewer.id] = viewer
	h.publishOutput([]byte("terminal output stays out of diagnostics"))
	if reason != "slow_viewer" || h.viewers[viewer.id] != nil {
		t.Fatal("slow-viewer eviction lost its bounded disconnect reason")
	}
	if frame := disconnectFrame(reason); frame.Op != "error" || frame.Reason != reason || len(frame.Data) != 0 {
		t.Fatal("disconnect is not a static error classification")
	}
}
