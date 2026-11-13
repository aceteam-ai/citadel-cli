package jobs

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type amtInputConn struct {
	net.Conn
	writes      int
	deadline    time.Time
	deadlineErr error
	afterWrite  func(int)
}

func (c *amtInputConn) SetWriteDeadline(d time.Time) error { c.deadline = d; return c.deadlineErr }
func (c *amtInputConn) Write(p []byte) (int, error) {
	if !c.deadline.After(time.Now()) {
		return 0, context.DeadlineExceeded
	}
	c.writes++
	if c.afterWrite != nil {
		c.afterWrite(c.writes)
	}
	return len(p), nil
}
func (c *amtInputConn) Close() error { return nil }

func TestAMTKeyRenewsExpiredHandshakeDeadline(t *testing.T) {
	conn := &amtInputConn{deadline: time.Now().Add(-time.Second)}
	r := &amtRFB{conn: conn}
	before := time.Now()
	if err := r.key(context.Background(), 'x', true); err != nil {
		t.Fatal(err)
	}
	if conn.writes != 1 || conn.deadline.Before(before.Add(29*time.Second)) || conn.deadline.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("key did not get a bounded fresh deadline: writes=%d deadline=%v", conn.writes, conn.deadline)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	if err := r.key(ctx, 'x', false); err != nil {
		t.Fatal(err)
	}
	if !conn.deadline.Equal(want) {
		t.Fatal("write extended the parent deadline")
	}
	conn.deadlineErr = errors.New("synthetic deadline failure")
	if err := r.key(ctx, 'x', true); err == nil || conn.writes != 2 {
		t.Fatal("write continued without a deadline")
	}
}

func TestAMTInputCancellationDuringPacing(t *testing.T) {
	for _, kind := range []string{"keys", "type"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &amtInputConn{afterWrite: func(n int) {
				if kind == "keys" || n == 2 {
					cancel()
				}
			}}
			r := &amtRFB{conn: conn}
			var err error
			if kind == "keys" {
				_, err = r.keys(ctx, "ctrl+x")
			} else {
				err = r.typeText(ctx, "x")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pacing ignored cancellation: %v", err)
			}
			want := 2
			if kind == "keys" {
				want = 1
			}
			if conn.writes != want {
				t.Fatalf("writes after cancellation: got %d want %d", conn.writes, want)
			}
		})
	}
}

func TestAMTInputRefusesBeforeWriting(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("x", 4097), "x\x00", "xé"} {
		conn := &amtInputConn{}
		r := &amtRFB{conn: conn}
		if err := r.typeText(context.Background(), text); err == nil || conn.writes != 0 {
			t.Fatal("invalid input wrote key events")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &amtInputConn{}
	r := &amtRFB{conn: conn}
	if err := r.typeText(ctx, strings.Repeat("x", 4096)); !errors.Is(err, context.Canceled) || conn.writes != 0 {
		t.Fatalf("4096-byte input must reach the cancellation gate, not length refusal: %v", err)
	}
	if _, err := r.keys(context.Background(), "ctrl+unsupported"); err == nil || conn.writes != 0 {
		t.Fatal("invalid combo wrote keys")
	}
}

type amtBlockingInputConn struct {
	net.Conn
	started chan struct{}
}

func (c *amtBlockingInputConn) Write(p []byte) (int, error) {
	close(c.started)
	return c.Conn.Write(p)
}

func TestAMTBlockedKeyWriteClosesOnCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	defer client.Close()
	conn := &amtBlockingInputConn{Conn: client, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&amtRFB{conn: conn}).key(ctx, 'x', true) }()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("key write never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled key write stayed blocked")
	}
}

func TestAMTBootFailureRestoresOriginalSettings(t *testing.T) {
	for _, stage := range []string{"Put", "ChangeBootOrder", "SetBootConfigRole", "RequestPowerStateChange"} {
		for _, mode := range []string{"transport", "refusal", "cancel"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				a, err := newAMTWSMan("10.0.0.2", "admin", "synthetic")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var actions, bodies []string
				failed := false
				a.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Authorization") == "" {
						return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="synthetic", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
					}
					raw, _ := io.ReadAll(req.Body)
					body := string(raw)
					action := body[strings.Index(body, "<a:Action>")+len("<a:Action>") : strings.Index(body, "</a:Action>")]
					action = action[strings.LastIndex(action, "/")+1:]
					actions = append(actions, action)
					bodies = append(bodies, body)
					if action == stage && !failed {
						failed = true
						if mode == "cancel" {
							cancel()
							return nil, context.Canceled
						}
						if mode == "transport" {
							return nil, errors.New("synthetic transport failure")
						}
						// A Put has no ReturnValue; its HTTP refusal must still trigger rollback.
						if stage == "Put" {
							return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
						}
						return amtSOAPFixture(`<h:ReturnValue xmlns:h="urn:test">1</h:ReturnValue>`), nil
					}
					if failed {
						if req.Context().Err() != nil {
							t.Fatal("rollback inherited cancelled context")
						}
						deadline, ok := req.Context().Deadline()
						if !ok || time.Until(deadline) > 5*time.Second {
							t.Fatal("rollback has no bounded cleanup deadline")
						}
					}
					if action == "Get" {
						return amtSOAPFixture(`<p:AMT_BootSettingData xmlns:p="http://intel.com/wbem/wscim/1/amt-schema/1/AMT_BootSettingData"><p:InstanceID>original-id</p:InstanceID><p:BIOSSetup>false</p:BIOSSetup><p:Other>preserved</p:Other></p:AMT_BootSettingData>`), nil
					}
					return amtSOAPFixture(`<h:ReturnValue xmlns:h="urn:test">0</h:ReturnValue>`), nil
				})
				if err := a.bootToSetup(ctx); err == nil {
					t.Fatal("failed setup reported success")
				}
				if !failed || len(actions) < 3 || actions[len(actions)-1] != "Put" {
					t.Fatalf("no restoration after %s: %v", stage, actions)
				}
				restore := bodies[len(bodies)-1]
				if !strings.Contains(restore, "<p:BIOSSetup>false</p:BIOSSetup>") || !strings.Contains(restore, "<p:Other>preserved</p:Other>") || !strings.Contains(restore, "original-id") {
					t.Fatal("rollback did not restore the original instance")
				}
			})
		}
	}
}

func TestAMTBootRestorationFailureIsReported(t *testing.T) {
	a, err := newAMTWSMan("10.0.0.2", "admin", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	puts := 0
	a.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="synthetic", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		raw, _ := io.ReadAll(req.Body)
		body := string(raw)
		if strings.Contains(body, "<a:Action>"+transferNS+"Get</a:Action>") {
			return amtSOAPFixture(`<p:AMT_BootSettingData><p:BIOSSetup>false</p:BIOSSetup></p:AMT_BootSettingData>`), nil
		}
		if strings.Contains(body, "<a:Action>"+transferNS+"Put</a:Action>") {
			puts++
		}
		return nil, errors.New("synthetic failed response")
	})
	err = a.bootToSetup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restoration unconfirmed") || puts != 2 {
		t.Fatalf("uncertain restore hidden: puts=%d err=%v", puts, err)
	}
}
func amtSOAPFixture(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` + body + `</s:Body></s:Envelope>`))}
}
