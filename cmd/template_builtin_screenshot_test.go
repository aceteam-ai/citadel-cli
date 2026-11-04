package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/gorilla/websocket"
)

func TestDecodePapercraftScreenshotPreservesExactLimitsAndPadding(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		data  []byte
	}{
		{"two_padding_bytes", 7, []byte("1234567")},
		{"one_padding_byte", 8, []byte("12345678")},
		{"no_padding", 9, []byte("123456789")},
		{"below_limit", 9, []byte("1234567")},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(test.data)
			got, err := decodePapercraftScreenshot(encoded, test.limit)
			if err != nil || !bytes.Equal(got, test.data) {
				t.Fatalf("bounded decode = %q %v, want %q", got, err, test.data)
			}
		})
	}
	// Preserve the standard decoder's newline handling when raw encoded bytes
	// still fit the fixed ceiling (line breaks do consume that ceiling).
	got, err := decodePapercraftScreenshot("YWJj\r\n", 5)
	if err != nil || string(got) != "abc" {
		t.Fatalf("newline decode = %q %v", got, err)
	}
}

func TestDecodePapercraftScreenshotRejectsSizeBeforeBase64Errors(t *testing.T) {
	// Syntax is invalid too. Observing the size sentinel, rather than a base64
	// error, proves these refusals happen before calling DecodeString.
	for _, test := range []struct {
		name    string
		encoded string
		limit   int
	}{
		{"encoded_ceiling", strings.Repeat("!", base64.StdEncoding.EncodedLen(8)+1), 8},
		{"same_encoded_length", "!!!!", 2},
		{"same_encoded_length_two_padding", "!!!!", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodePapercraftScreenshot(test.encoded, test.limit)
			if got != nil || !errors.Is(err, errPapercraftScreenshotTooLarge) {
				t.Fatalf("oversized invalid data reached decoding: %q %v", got, err)
			}
		})
	}
	for _, limit := range []int{1, 2, 7, 8} {
		exact := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, limit))
		over := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, limit+1))
		if len(exact) != len(over) {
			t.Fatalf("fixture does not share encoded length at limit %d", limit)
		}
		got, err := decodePapercraftScreenshot(over, limit)
		if got != nil || !errors.Is(err, errPapercraftScreenshotTooLarge) {
			t.Fatalf("same-length oversized decoded data accepted at limit %d: %v", limit, err)
		}
	}
}

func TestDecodePapercraftScreenshotMalformedMissingAndInvalidLimits(t *testing.T) {
	for _, encoded := range []string{"!!!!", "AA=A", "A===", "AAA"} {
		got, err := decodePapercraftScreenshot(encoded, 9)
		var syntax base64.CorruptInputError
		if got != nil || err == nil || !errors.As(err, &syntax) || errors.Is(err, errPapercraftScreenshotTooLarge) {
			t.Fatalf("in-bound malformed base64 %q = %q %v", encoded, got, err)
		}
	}
	got, err := decodePapercraftScreenshot("", 8)
	if got != nil || err == nil || !strings.Contains(err.Error(), "returned no data") {
		t.Fatalf("missing screenshot = %q %v", got, err)
	}
	for _, limit := range []int{-1, 0, papercraftMaxPNG + 1} {
		got, err := decodePapercraftScreenshot("eA==", limit)
		if got != nil || err == nil || !strings.Contains(err.Error(), "limit is invalid") {
			t.Fatalf("invalid limit %d = %q %v", limit, got, err)
		}
	}
}

func screenshotPageFixture(t *testing.T, result map[string]any) *chromiumRenderPage {
	t.Helper()
	up := websocket.Upgrader{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"type": "page", "webSocketDebuggerUrl": "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/fixture",
			}})
		case "/devtools/page/fixture":
			conn, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				var request struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := conn.ReadJSON(&request); err != nil {
					return
				}
				if request.Method != "Page.captureScreenshot" {
					_ = conn.WriteJSON(map[string]any{"id": request.ID, "error": map[string]any{"message": "unexpected fixture command"}})
					continue
				}
				if err := conn.WriteJSON(map[string]any{"id": request.ID, "result": result}); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client, err := cobrowsestream.DialCDP(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return &chromiumRenderPage{cdp: client}
}

func TestCapturePNGRoutesFakeCDPResultThroughBoundedDecoder(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfixture")
	page := screenshotPageFixture(t, map[string]any{"data": base64.StdEncoding.EncodeToString(png)})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := page.CapturePNG(ctx)
	if err != nil || !bytes.Equal(got, png) {
		t.Fatalf("CapturePNG = %q %v, want %q", got, err, png)
	}
}

func TestCapturePNGFakeCDPMalformedAndMissingDataFail(t *testing.T) {
	for _, data := range []any{"!", "", 123} {
		page := screenshotPageFixture(t, map[string]any{"data": data})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		got, err := page.CapturePNG(ctx)
		cancel()
		if got != nil || err == nil {
			t.Fatalf("invalid CDP screenshot %v = %q %v", data, got, err)
		}
	}
}
