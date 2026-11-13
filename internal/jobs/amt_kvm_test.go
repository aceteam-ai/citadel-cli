package jobs

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestAMTCredentialRequiresEnrolledDeviceAndSendsBothProofs(t *testing.T) {
	const capability = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	requests := 0
	const origin = "https://aceteam.test"
	client := &http.Client{Transport: amtRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/api/fabric/amt/credential" || r.Header.Get("Authorization") != "Bearer act_synthetic_device" || r.Header.Get("X-AMT-Capability") != capability {
			t.Errorf("credential request did not carry enrolled device and capability")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"host":"192.168.2.10","username":"admin","password":"synthetic"}`)), Header: make(http.Header)}, nil
	})}
	h := &AMTKVMHandler{Origin: func() string { return origin }, HTTPClient: client}
	job := &nexus.Job{Payload: map[string]string{
		"host": "192.168.2.10", "credential_url": origin + "/api/fabric/amt/credential", "credential_token": capability,
	}}
	if _, err := h.credential(context.Background(), job); err == nil {
		t.Fatal("unenrolled device reached credential endpoint")
	}
	if requests != 0 {
		t.Fatal("unenrolled device sent a request")
	}
	h.DeviceCreds = func() config.DeviceCreds {
		return config.DeviceCreds{Token: "act_synthetic_device", APIBaseURL: origin}
	}
	credential, err := h.credential(context.Background(), job)
	if err != nil || credential.Password != "synthetic" || requests != 1 {
		t.Fatalf("enrolled request failed: credential=%+v requests=%d err=%v", credential, requests, err)
	}
}

func TestAMTPowerRequiresExplicitTargetAndActionConfirmation(t *testing.T) {
	h := &AMTKVMHandler{Origin: func() string { return "" }}
	for _, fields := range []map[string]string{
		{},
		{"confirm_target": "192.168.2.10"},
		{"confirm_action": "reset"},
		{"confirm_target": "192.168.2.11", "confirm_action": "reset"},
		{"confirm_target": "192.168.2.10", "confirm_action": "off"},
	} {
		payload := map[string]string{"host": "192.168.2.10", "operation": "power", "action": "reset"}
		for key, value := range fields {
			payload[key] = value
		}
		_, err := h.Execute(JobContext{Ctx: context.Background()}, &nexus.Job{Payload: payload})
		if err == nil || !strings.Contains(err.Error(), "requires explicit target and action confirmation") {
			t.Fatalf("power fields %#v reached credential lookup: %v", fields, err)
		}
	}
	for _, action := range []string{"", "force_off", "unknown"} {
		_, err := h.Execute(JobContext{Ctx: context.Background()}, &nexus.Job{Payload: map[string]string{
			"host": "192.168.2.10", "operation": "power", "action": action,
			"confirm_target": "192.168.2.10", "confirm_action": action,
		}})
		if err == nil || err.Error() != "unsupported AMT power action" {
			t.Fatalf("unsupported action %q reached credential validation: %v", action, err)
		}
	}
	for _, action := range []string{"reset", "boot_to_setup"} {
		_, err := h.Execute(JobContext{Ctx: context.Background()}, &nexus.Job{Payload: map[string]string{
			"host": "192.168.2.10", "operation": "power", "action": action,
			"confirm_target": "192.168.2.10", "confirm_action": action,
		}})
		if err == nil || err.Error() != "AMT credential origin unavailable" {
			t.Fatalf("confirmed %s did not reach credential validation: %v", action, err)
		}
	}
}

func TestAMTOperationsSerializePerTarget(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h := &AMTKVMHandler{Origin: func() string {
		entered <- struct{}{}
		<-release
		return ""
	}}
	job := &nexus.Job{Payload: map[string]string{"host": "192.168.2.10", "operation": "status"}}
	var done sync.WaitGroup
	done.Add(2)
	go func() { defer done.Done(); _, _ = h.Execute(JobContext{}, job) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("first AMT operation did not enter credential lookup")
	}
	startedSecond := make(chan struct{})
	go func() {
		defer done.Done()
		close(startedSecond)
		_, _ = h.Execute(JobContext{}, job)
	}()
	<-startedSecond
	select {
	case <-entered:
		close(release)
		done.Wait()
		t.Fatal("same-target AMT operations overlapped")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	done.Wait()
	select {
	case <-entered:
	default:
		t.Fatal("second AMT operation never entered after first released")
	}
}

type amtRoundTrip func(*http.Request) (*http.Response, error)

func (f amtRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAMTLANAddress(t *testing.T) {
	for _, host := range []string{"10.0.0.2", "172.16.1.3", "192.168.2.10"} {
		if !amtLANAddress(host) {
			t.Errorf("private host %s refused", host)
		}
	}
	for _, host := range []string{"127.0.0.1", "100.64.0.1", "8.8.8.8", "192.168.2.10.evil", "192.168.2.010"} {
		if amtLANAddress(host) {
			t.Errorf("non-LAN host %s accepted", host)
		}
	}
}

func TestBootToSetupSequence(t *testing.T) {
	client, err := newAMTWSMan("192.168.2.10", "admin", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	var actions, bodies []string
	client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="nonce", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		body, _ := io.ReadAll(req.Body)
		text := string(body)
		bodies = append(bodies, text)
		action := text[strings.Index(text, "<a:Action>")+len("<a:Action>") : strings.Index(text, "</a:Action>")]
		actions = append(actions, action)
		response := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><h:ReturnValue xmlns:h="urn:test">0</h:ReturnValue></s:Body></s:Envelope>`
		if strings.HasSuffix(action, "Get") {
			response = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><p:AMT_BootSettingData xmlns:p="http://intel.com/wbem/wscim/1/amt-schema/1/"><p:InstanceID>Intel(r) AMT:BootSettingData 0</p:InstanceID><p:BIOSSetup>false</p:BIOSSetup></p:AMT_BootSettingData></s:Body></s:Envelope>`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	if err := client.bootToSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"Get", "Put", "ChangeBootOrder", "SetBootConfigRole", "RequestPowerStateChange"}
	if len(actions) != len(want) {
		t.Fatalf("actions = %v", actions)
	}
	for i, suffix := range want {
		if !strings.HasSuffix(actions[i], suffix) {
			t.Errorf("action %d = %s, want %s", i, actions[i], suffix)
		}
	}
	if !strings.Contains(bodies[1], "<p:BIOSSetup>true</p:BIOSSetup>") {
		t.Fatal("BIOSSetup was not set before boot order change")
	}
	if !strings.Contains(bodies[2], "<h:Source/>") {
		t.Fatal("boot source was not cleared")
	}
	if !strings.Contains(bodies[3], "<h:Role>1</h:Role>") {
		t.Fatal("one-time boot role was not set")
	}
	if !strings.Contains(bodies[4], "<h:PowerState>10</h:PowerState>") {
		t.Fatal("power reset was not last")
	}
}

func TestBootToSetupRestoresBIOSAfterLaterFailure(t *testing.T) {
	for _, failedAction := range []string{"ChangeBootOrder", "SetBootConfigRole", "RequestPowerStateChange"} {
		t.Run(failedAction, func(t *testing.T) {
			client, err := newAMTWSMan("192.168.2.10", "admin", "synthetic-password")
			if err != nil {
				t.Fatal(err)
			}
			var actions, bodies []string
			client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") == "" {
					return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="nonce", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				body, _ := io.ReadAll(req.Body)
				text := string(body)
				action := text[strings.Index(text, "<a:Action>")+len("<a:Action>") : strings.Index(text, "</a:Action>")]
				actions = append(actions, action)
				bodies = append(bodies, text)
				response := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><h:ReturnValue xmlns:h="urn:test">0</h:ReturnValue></s:Body></s:Envelope>`
				if strings.HasSuffix(action, "Get") {
					response = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><p:AMT_BootSettingData xmlns:p="http://intel.com/wbem/wscim/1/amt-schema/1/"><p:BIOSSetup>false</p:BIOSSetup></p:AMT_BootSettingData></s:Body></s:Envelope>`
				} else if strings.HasSuffix(action, failedAction) {
					response = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><h:ReturnValue xmlns:h="urn:test">1</h:ReturnValue></s:Body></s:Envelope>`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			if err := client.bootToSetup(context.Background()); err == nil {
				t.Fatal("failing step was accepted")
			}
			if len(actions) < 4 || !strings.HasSuffix(actions[len(actions)-1], "Put") {
				t.Fatalf("BIOS restore was not the last action: %v", actions)
			}
			if !strings.Contains(bodies[len(bodies)-1], "<p:BIOSSetup>false</p:BIOSSetup>") {
				t.Fatal("restore did not use the original BIOSSetup value")
			}
		})
	}
}

func TestBootToSetupRestoresAfterAmbiguousInitialPut(t *testing.T) {
	client, err := newAMTWSMan("192.168.2.10", "admin", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	putCount := 0
	client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="nonce", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		body, _ := io.ReadAll(req.Body)
		text := string(body)
		if strings.Contains(text, "Transfer/Get") || strings.Contains(text, transferNS+"Get") {
			response := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><p:AMT_BootSettingData xmlns:p="http://intel.com/wbem/wscim/1/amt-schema/1/"><p:BIOSSetup>false</p:BIOSSetup></p:AMT_BootSettingData></s:Body></s:Envelope>`
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
		}
		if strings.Contains(text, transferNS+"Put") {
			putCount++
			if putCount == 1 {
				httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
				return nil, errors.New("synthetic response lost after write")
			}
			if !strings.Contains(text, "<p:BIOSSetup>false</p:BIOSSetup>") {
				t.Error("rollback did not restore the original BIOSSetup field")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		t.Errorf("unexpected AMT action after uncertain Put")
		return nil, errors.New("unexpected action")
	})
	if err := client.bootToSetup(context.Background()); !errors.Is(err, ErrAMTOutcomeUnknown) {
		t.Fatalf("initial Put lost its uncertain outcome: %v", err)
	}
	if putCount != 2 {
		t.Fatalf("expected arming attempt and rollback, got %d Put requests", putCount)
	}
}

func TestAMTTypeExtendsWriteDeadlineAndCancelsBetweenKeys(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	go func() { _, _ = io.Copy(io.Discard, right) }()
	if err := left.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	rfb := &amtRFB{conn: left}
	if err := rfb.typeText(context.Background(), strings.Repeat("x", 20)); err != nil {
		t.Fatalf("typing should outlast the session setup deadline: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rfb = &amtRFB{conn: left}
	if err := rfb.typeText(cancelled, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-write cancellation should remain a refusal: %v", err)
	}
	rfb = &amtRFB{conn: left}
	midway, stop := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, stop)
	start := time.Now()
	if err := rfb.typeText(midway, strings.Repeat("x", 20)); !errors.Is(err, ErrAMTOutcomeUnknown) {
		t.Fatalf("post-write cancellation needs an unknown outcome: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("typing did not promptly stop after cancellation")
	}
}

func TestAMTPowerOutcomeUnknownOnlyAfterWrite(t *testing.T) {
	for _, wrote := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "after-write"}[wrote], func(t *testing.T) {
			client, err := newAMTWSMan("192.168.2.10", "admin", "synthetic-password")
			if err != nil {
				t.Fatal(err)
			}
			client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") == "" {
					return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="nonce", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if wrote {
					httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
				}
				return nil, errors.New("synthetic transport break")
			})
			err = client.power(context.Background(), "reset")
			if errors.Is(err, ErrAMTOutcomeUnknown) != wrote {
				t.Fatalf("unknown outcome = %t, wrote = %t, err = %v", errors.Is(err, ErrAMTOutcomeUnknown), wrote, err)
			}
		})
	}
}

func TestAMTPowerAuthenticationRefusalRemainsDefinitive(t *testing.T) {
	client, err := newAMTWSMan("192.168.2.10", "admin", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="nonce", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		httptrace.ContextClientTrace(req.Context()).WroteHeaders()
		httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, trace := WithAMTMutationTrace(context.Background())
	err = client.power(ctx, "reset")
	if err == nil || errors.Is(err, ErrAMTOutcomeUnknown) || !strings.Contains(err.Error(), "authentication refused") {
		t.Fatalf("definitive authentication refusal was misclassified: %v", err)
	}
	if trace.Possible() {
		t.Fatal("definitive authentication refusal left a possible mutation for the watchdog")
	}
}

func TestAMTWSManTraceVisibleWhileWriteIsInFlight(t *testing.T) {
	client, err := newAMTWSMan("192.168.2.10", "admin", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	client.client.Transport = amtRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{`Digest realm="Intel AMT", nonce="nonce", qop="auth"`}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		httptrace.ContextClientTrace(req.Context()).WroteHeaders()
		close(entered)
		<-release
		return nil, errors.New("synthetic response loss")
	})
	ctx, trace := WithAMTMutationTrace(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.power(ctx, "reset") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("mutating write did not start")
	}
	if !trace.Possible() {
		close(release)
		t.Fatal("watchdog cannot observe the in-flight WS-MAN write")
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrAMTOutcomeUnknown) || !trace.Possible() {
		t.Fatalf("written request lost its unknown outcome: %v", err)
	}
}
