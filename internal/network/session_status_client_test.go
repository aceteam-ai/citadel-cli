package network

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type sessionStatusRoundTripper func(*http.Request) (*http.Response, error)

func (f sessionStatusRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSessionStatusClientRejectsRedirectWithoutChangingLegacyClient(t *testing.T) {
	for _, rejectRedirect := range []bool{true, false} {
		calls := 0
		transport := sessionStatusRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Method != http.MethodGet || request.URL.Host != "citadel.local" || request.Header.Get("Authorization") != "" {
				t.Fatal("local status client changed method, authority or credential use")
			}
			if calls == 1 {
				if request.URL.Path != "/citadel/session/v1/status" {
					t.Fatal("initial request was not the existing session status route")
				}
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://citadel.local/redirected"}}, Body: io.NopCloser(strings.NewReader("private redirect body"))}, nil
			}
			if rejectRedirect || calls != 2 || request.URL.Path != "/redirected" {
				t.Fatal("fixed viewer followed a redirect or legacy redirect behavior changed")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})
		client := sessionControlHTTPClient(transport, rejectRedirect)
		var response *http.Response
		var err error
		if rejectRedirect {
			response, err = sessionControlStatusRequestWithClient(context.Background(), client)
		} else {
			response, err = sessionControlRequestWithClient(context.Background(), client, http.MethodGet, "/citadel/session/v1/status")
		}
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if rejectRedirect && (calls != 1 || response.StatusCode != http.StatusFound) || !rejectRedirect && (calls != 2 || response.StatusCode != http.StatusOK) {
			t.Fatal("redirect policy did not preserve the per-call boundary")
		}
	}
}
