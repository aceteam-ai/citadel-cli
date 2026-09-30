package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type exchangeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f exchangeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func exchangeHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHTTPExchangerSuccessSendsBearerAndJSON(t *testing.T) {
	transport := exchangeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if req.URL.String() != "https://control.example/ingress/exchange" {
			t.Fatalf("URL = %q", req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer node-secret" {
			t.Fatalf("Authorization = %q, want bearer", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		var got exchangeRequest
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		want := exchangeRequest{Slug: "app", Code: "one-time-code", Nonce: "browser-nonce"}
		if got != want {
			t.Fatalf("request = %+v, want %+v", got, want)
		}
		return exchangeHTTPResponse(req, http.StatusOK, `{"credential":"app-credential","max_age":300,"next":"/docs?q=1"}`), nil
	})

	exchanger := NewHTTPExchanger("https://control.example/ingress/exchange", "node-secret", &http.Client{Transport: transport})
	result, status, err := exchanger.Exchange(context.Background(), "app", "one-time-code", "browser-nonce")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	want := ExchangeResult{Credential: "app-credential", MaxAge: 300, Next: "/docs?q=1"}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
}

func TestHTTPExchangerReturnsHTTPFailureStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusUnprocessableEntity,
		499,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		599,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			transport := exchangeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return exchangeHTTPResponse(req, status, `{"credential":"must-not-be-used","max_age":300,"next":"/"}`), nil
			})
			exchanger := NewHTTPExchanger("https://control.example/exchange", "token", &http.Client{Transport: transport})
			result, gotStatus, err := exchanger.Exchange(context.Background(), "app", "code", "nonce")
			if err != nil {
				t.Fatalf("Exchange() error = %v, want status-only classification", err)
			}
			if gotStatus != status {
				t.Fatalf("status = %d, want %d", gotStatus, status)
			}
			if result != (ExchangeResult{}) {
				t.Fatalf("non-200 result was trusted: %+v", result)
			}
		})
	}
}

func TestHTTPExchangerNeverFollowsRedirects(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var targetHits, callerRedirectCalls int
			var targetAuthorization, targetBody string
			callerPolicyErr := errors.New("caller redirect policy")
			transport := exchangeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/target" {
					targetHits++
					targetAuthorization = req.Header.Get("Authorization")
					body, _ := io.ReadAll(req.Body)
					targetBody = string(body)
					return exchangeHTTPResponse(req, http.StatusOK, `{"credential":"stolen","max_age":300,"next":"/"}`), nil
				}
				resp := exchangeHTTPResponse(req, status, `{"credential":"redirect-body","max_age":300,"next":"/"}`)
				resp.Header.Set("Location", "https://control.example/target")
				return resp, nil
			})
			caller := &http.Client{
				Timeout:   37 * time.Second,
				Transport: transport,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					callerRedirectCalls++
					return callerPolicyErr
				},
			}

			exchanger := NewHTTPExchanger("https://control.example/exchange", "node-secret", caller)
			result, gotStatus, err := exchanger.Exchange(context.Background(), "app", "one-time-code", "browser-nonce")
			if err == nil || !strings.Contains(err.Error(), "unexpected status") {
				t.Fatalf("redirect error = %v, want unexpected-status refusal", err)
			}
			if gotStatus != status || result != (ExchangeResult{}) {
				t.Fatalf("redirect result/status = (%+v, %d), want zero/%d", result, gotStatus, status)
			}
			if targetHits != 0 || targetAuthorization != "" || targetBody != "" {
				t.Fatalf("redirect target received request/secrets: hits=%d authorization=%q body=%q", targetHits, targetAuthorization, targetBody)
			}
			if callerRedirectCalls != 0 {
				t.Fatalf("caller's redirect policy was invoked %d times", callerRedirectCalls)
			}
			if got := caller.CheckRedirect(nil, nil); !errors.Is(got, callerPolicyErr) {
				t.Fatalf("caller's redirect policy was mutated: %v", got)
			}
			if caller.Timeout != 37*time.Second {
				t.Fatalf("caller's timeout was mutated: %v", caller.Timeout)
			}
			if internal := exchanger.(*httpExchanger); internal.client == caller {
				t.Fatal("exchanger retained caller's client instead of a clone")
			} else if internal.client.Timeout != 10*time.Second {
				t.Fatalf("exchange timeout = %v, want hard 10s ceiling", internal.client.Timeout)
			}
		})
	}
}

func TestHTTPExchangerClientCopyAndTimeout(t *testing.T) {
	defaultClient := NewHTTPExchanger("https://control.example/exchange", "", nil).(*httpExchanger).client
	if defaultClient.Timeout != 10*time.Second {
		t.Fatalf("default timeout = %v, want 10s", defaultClient.Timeout)
	}
	if err := defaultClient.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("default redirect policy = %v, want ErrUseLastResponse", err)
	}

	transport := exchangeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	caller := &http.Client{Timeout: 50 * time.Millisecond, Transport: transport}
	exchanger := NewHTTPExchanger("https://control.example/exchange", "", caller)
	started := time.Now()
	result, status, err := exchanger.Exchange(context.Background(), "app", "code", "nonce")
	if err == nil || result != (ExchangeResult{}) || status != 0 {
		t.Fatalf("timeout result/status/error = (%+v, %d, %v), want zero/0/error", result, status, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("50ms client timeout took %v", elapsed)
	}
	if caller.Timeout != 50*time.Millisecond || caller.Transport == nil || caller.CheckRedirect != nil {
		t.Fatalf("caller client was mutated: timeout=%v transport=%T redirect_nil=%v", caller.Timeout, caller.Transport, caller.CheckRedirect == nil)
	}
}

func TestHTTPExchangerRejectsMalformedOrOversizedSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "invalid_json", body: `{`},
		{name: "trailing_json", body: `{"credential":"c","max_age":1,"next":"/"} {}`},
		{name: "missing_credential", body: `{"max_age":300,"next":"/"}`},
		{name: "zero_max_age", body: `{"credential":"c","max_age":0,"next":"/"}`},
		{name: "negative_max_age", body: `{"credential":"c","max_age":-1,"next":"/"}`},
		{name: "missing_next", body: `{"credential":"c","max_age":300}`},
		{name: "wrong_type", body: `{"credential":"c","max_age":"300","next":"/"}`},
		{name: "oversized", body: strings.Repeat(" ", exchangeResponseMaxBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := exchangeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return exchangeHTTPResponse(req, http.StatusOK, tc.body), nil
			})
			exchanger := NewHTTPExchanger("https://control.example/exchange", "", &http.Client{Transport: transport})
			result, status, err := exchanger.Exchange(context.Background(), "app", "code", "nonce")
			if err == nil {
				t.Fatalf("malformed response was accepted: %+v", result)
			}
			if status != http.StatusOK || result != (ExchangeResult{}) {
				t.Fatalf("result/status = (%+v, %d), want zero/200", result, status)
			}
		})
	}
}

func TestHTTPExchangerTransportFailure(t *testing.T) {
	wantErr := errors.New("network unavailable")
	transport := exchangeRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	})
	exchanger := NewHTTPExchanger("https://control.example/exchange", "", &http.Client{Transport: transport})
	result, status, err := exchanger.Exchange(context.Background(), "app", "code", "nonce")
	if !errors.Is(err, wantErr) || status != 0 || result != (ExchangeResult{}) {
		t.Fatalf("transport result/status/error = (%+v, %d, %v)", result, status, err)
	}
}

func TestDeriveExchangeURL(t *testing.T) {
	for _, tc := range []struct {
		name, routesURL, want string
	}{
		{name: "normal", routesURL: "https://cp.example.com/ingress/routes", want: "https://cp.example.com/ingress/exchange"},
		{name: "trailing_slash", routesURL: "https://cp.example.com/ingress/routes/", want: "https://cp.example.com/ingress/exchange"},
		{name: "root", routesURL: "https://cp.example.com/routes", want: "https://cp.example.com/exchange"},
		{name: "relative", routesURL: "routes", want: "/exchange"},
		{name: "query_and_fragment", routesURL: "https://cp.example.com/ingress/routes?node=n1#fragment", want: "https://cp.example.com/ingress/exchange?node=n1#fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveExchangeURL(tc.routesURL)
			if err != nil || got != tc.want {
				t.Fatalf("DeriveExchangeURL(%q) = (%q, %v), want %q", tc.routesURL, got, err, tc.want)
			}
		})
	}
	if _, err := DeriveExchangeURL("https://control.example/%zz"); err == nil {
		t.Fatal("invalid routes URL was accepted")
	}
}
