package jobs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

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
	_, err := h.Execute(JobContext{Ctx: context.Background()}, &nexus.Job{Payload: map[string]string{
		"host": "192.168.2.10", "operation": "power", "action": "reset",
		"confirm_target": "192.168.2.10", "confirm_action": "reset",
	}})
	if err == nil || err.Error() != "AMT credential origin unavailable" {
		t.Fatalf("confirmed power did not reach credential validation: %v", err)
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
