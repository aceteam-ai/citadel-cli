package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"
)

// No browser is launched: a placeholder Cmd marks the host driver's state,
// and the only server is a loopback fake inside the network-isolated test.
func TestMeetingPhaseContextMethodsCancelBlockedDiscovery(t *testing.T) {
	for _, kind := range []string{"host", "container"} {
		for _, method := range []string{"url", "navigate", "evaluate", "type"} {
			t.Run(kind+"/"+method, func(t *testing.T) {
				started, release := make(chan struct{}, 1), make(chan struct{})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					started <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer srv.Close()
				defer close(release)
				port := srv.Listener.Addr().(*net.TCPAddr).Port
				var b interface {
					CurrentURLContext(context.Context) (string, error)
					NavigateContext(context.Context, string) error
					EvaluateContext(context.Context, string) (any, error)
					TypeContext(context.Context, string, string) error
				}
				if kind == "host" {
					b = &MeetingBrowser{cmd: &exec.Cmd{}, debugPort: port}
				} else {
					b = NewCDPBrowser(port)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					var err error
					switch method {
					case "url":
						_, err = b.CurrentURLContext(ctx)
					case "navigate":
						err = b.NavigateContext(ctx, "https://meet.google.com/synthetic")
					case "evaluate":
						_, err = b.EvaluateContext(ctx, "1")
					case "type":
						err = b.TypeContext(ctx, "textarea", "fixture")
					}
					done <- err
				}()
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("discovery did not start")
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatal("phase cancellation lost", err)
					}
				case <-time.After(time.Second):
					t.Fatal("phase did not cancel blocked discovery")
				}
			})
		}
	}
}

func TestMeetingPhaseContextCancelsWhileBrowserMutexHeld(t *testing.T) {
	b := &MeetingBrowser{}
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := b.CurrentURLContext(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("browser mutex hid phase cancellation")
	}
}
