package jobs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
)

func TestAMTMutationTraceFreezesAndFencesFirstWrite(t *testing.T) {
	_, trace := WithAMTMutationTrace(context.Background())
	if trace.Close() || trace.Possible() {
		t.Fatal("prewrite close became uncertain")
	}
	if trace.BeginWrite() {
		t.Fatal("closed prewrite trace admitted a late first write")
	}
	trace.EndWrite(true)
	if trace.Close() || trace.Possible() {
		t.Fatal("late completion changed a frozen definite outcome")
	}
}

func TestAMTMutationTraceFreezesInflightAndRetainsBoundedRestoration(t *testing.T) {
	_, trace := WithAMTMutationTrace(context.Background())
	if !trace.BeginWrite() || !trace.Close() {
		t.Fatal("in-flight write lost its uncertain outcome")
	}
	trace.EndWrite(false) // an eventual auth refusal cannot revise a published result
	if !trace.Possible() || !trace.Close() {
		t.Fatal("late response changed the frozen outcome")
	}
	if !trace.BeginWrite() {
		t.Fatal("already-uncertain attempt cannot finish bounded restoration")
	}
	trace.EndWrite(false)
	if !trace.Possible() {
		t.Fatal("restoration changed the frozen uncertain result")
	}
	_, refused := WithAMTMutationTrace(context.Background())
	if !refused.BeginWrite() {
		t.Fatal("initial write refused")
	}
	refused.EndWrite(false)
	if refused.Close() {
		t.Fatal("completed authentication refusal was called uncertain")
	}
}

func TestAMTMutationTraceFirstWriteCloseRace(t *testing.T) {
	for i := 0; i < 1000; i++ {
		_, trace := WithAMTMutationTrace(context.Background())
		start := make(chan struct{})
		var began, possible bool
		var done sync.WaitGroup
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			began = trace.BeginWrite()
			if began {
				trace.EndWrite(true)
			}
		}()
		go func() { defer done.Done(); <-start; possible = trace.Close() }()
		close(start)
		done.Wait()
		if began != possible || trace.Possible() != possible || trace.Close() != possible {
			t.Fatal("first write and frozen classification were not atomic")
		}
	}
}

func TestAMTRFBLateFirstWriteIsBlocked(t *testing.T) {
	ctx, trace := WithAMTMutationTrace(context.Background())
	trace.Close()
	conn := &amtInputConn{}
	err := (&amtRFB{conn: conn}).key(ctx, 'x', true)
	if !errors.Is(err, context.Canceled) || conn.writes != 0 || trace.Possible() {
		t.Fatal("late first key event reached the connection")
	}
}

func TestAMTWSManLateFirstBodyIsBlocked(t *testing.T) {
	for _, headersCallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "body-only", true: "header-callback"}[headersCallback], func(t *testing.T) {
			ctx, trace := WithAMTMutationTrace(context.Background())
			trace.Close()
			client, err := newAMTWSMan("10.0.0.2", "admin", "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
				if req.GetBody != nil {
					t.Fatal("automatic request clone could bypass first-write guard")
				}
				if headersCallback {
					httptrace.ContextClientTrace(req.Context()).WroteHeaders()
				}
				data, readErr := io.ReadAll(req.Body)
				if len(data) != 0 {
					t.Fatal("late SOAP command bytes escaped the closed observer")
				}
				if !errors.Is(readErr, context.Canceled) {
					t.Fatal("late SOAP body did not refuse")
				}
				if ct := httptrace.ContextClientTrace(req.Context()); ct != nil && ct.WroteRequest != nil {
					ct.WroteRequest(httptrace.WroteRequestInfo{Err: readErr})
				}
				return nil, readErr
			})
			err = client.power(ctx, "reset")
			if err == nil || errors.Is(err, ErrAMTOutcomeUnknown) || trace.Possible() {
				t.Fatalf("closed prewrite SOAP was not a definite refusal: %v", err)
			}
		})
	}
}

func TestAMTTraceAllowsReadOnlyBodyAfterFreeze(t *testing.T) {
	ctx, trace := WithAMTMutationTrace(context.Background())
	trace.Close()
	client, err := newAMTWSMan("10.0.0.2", "admin", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != nil || !strings.Contains(string(data), transferNS+"Get") {
			t.Fatal("read-only request was changed by the mutation fence")
		}
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="synthetic", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return amtSOAPFixture(`<p:Result xmlns:p="urn:test">ok</p:Result>`), nil
	})
	if _, err := client.get(ctx, amtNS+"AMT_SetupAndConfigurationService"); err != nil || trace.Possible() {
		t.Fatalf("read-only request failed or set a mutation: %v", err)
	}
}
