package cdpdiscovery

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListCompleteNeverOmitsAnUnusablePage(t *testing.T) {
	for _, endpoint := range []string{"", "invalid", "ws://user:pass@localhost/devtools/page/b"} {
		body := `[{"id":"a","type":"page","webSocketDebuggerUrl":"ws://localhost/devtools/page/a"},{"id":"b","type":"page","webSocketDebuggerUrl":"` + endpoint + `"}]`
		client := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if targets, err := ListComplete(context.Background(), 9876, client); err == nil || len(targets) != 0 {
			t.Fatal("complete discovery silently dropped a page")
		}
		if targets, err := List(context.Background(), 9876, client); err != nil || len(targets) != 1 {
			t.Fatal("legacy discovery's omission contract changed")
		}
	}
}
