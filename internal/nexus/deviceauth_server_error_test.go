package nexus

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tokenStatusServer answers the token endpoint with statuses in order (the
// last repeats). 200 returns an authkey.
func tokenStatusServer(t *testing.T, statuses []int, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := statuses[min(*calls, len(statuses)-1)]
		*calls++
		if status == http.StatusOK {
			fmt.Fprint(w, `{"authkey":"registration-key"}`)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"Internal server error"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The server can answer 500 while an approval is being committed. That is
// not a verdict on the grant, so the poll must keep going.
func TestPollForTokenRetriesServerErrors(t *testing.T) {
	calls := 0
	srv := tokenStatusServer(t, []int{500, 502, 503, 200}, &calls)
	token, err := NewDeviceAuthClient(srv.URL).PollForToken("grant", 0)
	if err != nil || token == nil || token.Authkey != "registration-key" || calls != 4 {
		t.Fatalf("token=%v err=%v calls=%d", token, err, calls)
	}
}

func TestPollForTokenGivesUpOnPersistentServerErrors(t *testing.T) {
	calls := 0
	srv := tokenStatusServer(t, []int{500}, &calls)
	_, err := NewDeviceAuthClient(srv.URL).PollForToken("grant", 0)
	if err == nil || !strings.Contains(err.Error(), "500") || calls != MaxConsecutiveTokenServerErrors+1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
