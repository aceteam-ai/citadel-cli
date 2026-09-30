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

type httpSourceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f httpSourceRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpSourceResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHTTPSourcesRejectRedirectsWithoutForwardingSecrets(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprintf("routes_%d", status), func(t *testing.T) {
			var targetHits int
			var targetAuthorization, targetCookie, targetBody string
			callerRedirectErr := errors.New("caller's redirect policy")
			callerRedirectCalls := 0
			caller := &http.Client{
				Timeout: 23 * time.Second,
				Transport: httpSourceRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/target" {
						targetHits++
						targetAuthorization = req.Header.Get("Authorization")
						targetCookie = req.Header.Get("Cookie")
						body, _ := io.ReadAll(req.Body)
						targetBody = string(body)
						return httpSourceResponse(req, http.StatusOK, `{"routes":{}}`), nil
					}
					if got := req.Header.Get("Authorization"); got != "Bearer routes-secret" {
						t.Fatalf("initial Authorization = %q, want bearer", got)
					}
					resp := httpSourceResponse(req, status, `{"routes":{"redirected":{"mesh_ip":"100.64.0.1","port":8080}}}`)
					resp.Header.Set("Location", "https://control.example/target")
					return resp, nil
				}),
				CheckRedirect: func(*http.Request, []*http.Request) error {
					callerRedirectCalls++
					return callerRedirectErr
				},
			}

			source := NewHTTPRoutesSource("https://control.example/routes", "routes-secret", caller)
			gotStatus, etag, body, err := source.Fetch(context.Background(), "")
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if gotStatus != status {
				t.Fatalf("Fetch() status = %d, want original redirect %d", gotStatus, status)
			}
			if _, err := applyRoutesResponse(nil, gotStatus, etag, body, time.Now()); err == nil {
				t.Fatalf("redirect status %d was accepted as a routes response", status)
			}
			oneStatus, _, err := source.FetchOne(context.Background(), "redirected")
			if err != nil {
				t.Fatalf("FetchOne() error = %v", err)
			}
			if oneStatus != status {
				t.Fatalf("FetchOne() status = %d, want original redirect %d", oneStatus, status)
			}
			if targetHits != 0 || targetAuthorization != "" || targetCookie != "" || targetBody != "" {
				t.Fatalf("redirect target received request/secrets: hits=%d authorization=%q cookie=%q body=%q", targetHits, targetAuthorization, targetCookie, targetBody)
			}
			if callerRedirectCalls != 0 {
				t.Fatalf("caller's CheckRedirect was used by cloned client %d times", callerRedirectCalls)
			}
			if got := caller.CheckRedirect(nil, nil); !errors.Is(got, callerRedirectErr) {
				t.Fatalf("caller's CheckRedirect was mutated: got %v", got)
			}
			if caller.Timeout != 23*time.Second {
				t.Fatalf("caller's Timeout was mutated: %v", caller.Timeout)
			}
			if internal := source.(*httpRoutesSource); internal.client == caller {
				t.Fatal("routes source retained caller's client instead of a clone")
			}
		})

		t.Run(fmt.Sprintf("authz_%d", status), func(t *testing.T) {
			var targetHits int
			var targetAuthorization, targetCookie, targetBody string
			callerRedirectErr := errors.New("caller's redirect policy")
			callerRedirectCalls := 0
			caller := &http.Client{
				Timeout: 29 * time.Second,
				Transport: httpSourceRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/target" {
						targetHits++
						targetAuthorization = req.Header.Get("Authorization")
						targetCookie = req.Header.Get("Cookie")
						body, _ := io.ReadAll(req.Body)
						targetBody = string(body)
						return httpSourceResponse(req, http.StatusOK, `{"allow":true,"subject":"redirected"}`), nil
					}
					if got := req.Header.Get("Authorization"); got != "Bearer authz-secret" {
						t.Fatalf("initial Authorization = %q, want bearer", got)
					}
					resp := httpSourceResponse(req, status, `{"allow":true,"subject":"attacker"}`)
					resp.Header.Set("Location", "https://control.example/target")
					return resp, nil
				}),
				CheckRedirect: func(*http.Request, []*http.Request) error {
					callerRedirectCalls++
					return callerRedirectErr
				},
			}

			authorizer := NewHTTPAuthorizer("https://control.example/authz", "authz-secret", caller)
			decision, err := authorizer.Authorize(context.Background(), "app", "session=secret")
			if err == nil || decision.Allow || decision.Subject != "" {
				t.Fatalf("redirect status %d was trusted: allow=%v subject=%q err=%v", status, decision.Allow, decision.Subject, err)
			}
			if targetHits != 0 || targetAuthorization != "" || targetCookie != "" || targetBody != "" {
				t.Fatalf("redirect target received request/secrets: hits=%d authorization=%q cookie=%q body=%q", targetHits, targetAuthorization, targetCookie, targetBody)
			}
			if callerRedirectCalls != 0 {
				t.Fatalf("caller's CheckRedirect was used by cloned client %d times", callerRedirectCalls)
			}
			if got := caller.CheckRedirect(nil, nil); !errors.Is(got, callerRedirectErr) {
				t.Fatalf("caller's CheckRedirect was mutated: got %v", got)
			}
			if caller.Timeout != 29*time.Second {
				t.Fatalf("caller's Timeout was mutated: %v", caller.Timeout)
			}
			if internal := authorizer.(*httpAuthorizer); internal.client == caller {
				t.Fatal("authorizer retained caller's client instead of a clone")
			}
		})
	}
}

func TestHTTPSourcesNilClientsDisableRedirects(t *testing.T) {
	routesClient := NewHTTPRoutesSource("https://control.example/routes", "", nil).(*httpRoutesSource).client
	if routesClient.Timeout != 15*time.Second {
		t.Fatalf("routes default timeout = %v, want 15s", routesClient.Timeout)
	}
	if err := routesClient.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("routes default redirect policy = %v, want ErrUseLastResponse", err)
	}

	authzClient := NewHTTPAuthorizer("https://control.example/authz", "", nil).(*httpAuthorizer).client
	if authzClient.Timeout != 10*time.Second {
		t.Fatalf("authz default timeout = %v, want 10s", authzClient.Timeout)
	}
	if err := authzClient.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("authz default redirect policy = %v, want ErrUseLastResponse", err)
	}
}

func TestHTTPSourcesPreserveNormalResponses(t *testing.T) {
	routesTransport := httpSourceRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer routes-secret" {
			t.Fatalf("routes Authorization = %q, want bearer", got)
		}
		if req.Header.Get("If-None-Match") == "fresh" {
			return httpSourceResponse(req, http.StatusNotModified, ""), nil
		}
		resp := httpSourceResponse(req, http.StatusOK, `{"routes":{}}`)
		resp.Header.Set("ETag", "fresh")
		return resp, nil
	})
	source := NewHTTPRoutesSource("https://control.example/routes", "routes-secret", &http.Client{Transport: routesTransport})
	status, etag, body, err := source.Fetch(context.Background(), "")
	if err != nil || status != http.StatusOK || etag != "fresh" || string(body) != `{"routes":{}}` {
		t.Fatalf("normal 200: status=%d etag=%q body=%q err=%v", status, etag, body, err)
	}
	status, _, body, err = source.Fetch(context.Background(), "fresh")
	if err != nil || status != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("normal 304: status=%d body=%q err=%v", status, body, err)
	}
	status, body, err = source.FetchOne(context.Background(), "late")
	if err != nil || status != http.StatusOK || string(body) != `{"routes":{}}` {
		t.Fatalf("normal FetchOne 200: status=%d body=%q err=%v", status, body, err)
	}

	authzTransport := httpSourceRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer authz-secret" {
			t.Fatalf("authz Authorization = %q, want bearer", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("authz Content-Type = %q, want application/json", got)
		}
		var got authzRequest
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			t.Fatalf("decode authz request: %v", err)
		}
		if got.Slug != "app" || got.Cookie != "session=secret" {
			t.Fatalf("authz request = %+v", got)
		}
		return httpSourceResponse(req, http.StatusOK, `{"allow":true,"subject":"user-1","reason":"revoked"}`), nil
	})
	authorizer := NewHTTPAuthorizer("https://control.example/authz", "authz-secret", &http.Client{Transport: authzTransport})
	decision, err := authorizer.Authorize(context.Background(), "app", "session=secret")
	if err != nil || !decision.Allow || decision.Subject != "user-1" || decision.Reason != "revoked" {
		t.Fatalf("normal authz 200: decision=%+v err=%v", decision, err)
	}
}
