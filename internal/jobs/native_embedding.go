package jobs

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// nativeEmbeddingBinding is a protocol binding, not binary/model admission.
// Only the constructor below creates it; no payload or environment selects it.
type nativeEmbeddingBinding struct {
	endpoint   string
	model      string
	dimensions int
}

// NewNativeEmbeddingHandler constructs the protocol adapter for a separately
// admitted native embedding service. It DOES NOT verify, download or start a
// binary/model, discover an engine, or grant permission to use one. Future
// lifecycle wiring must first establish the pinned, signature-verified runtime
// and pinned model, then bind its exact API alias and dimension here.
//
// This constructor currently has no production caller. The ordinary worker
// continues to construct the zero-value TEI handler.
func NewNativeEmbeddingHandler(endpoint, model string, dimensions int) (*EmbeddingHandler, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Host == "" || u.Path != "" || u.RawPath != "" || strings.ContainsAny(endpoint, "?#\\\r\n\t") {
		return nil, errors.New("native embedding endpoint must be a literal loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, errors.New("native embedding endpoint must be a literal loopback HTTP origin with a port")
	}
	if model == "" || len(model) > 256 || dimensions < 1 || dimensions > 65536 {
		return nil, errors.New("native embedding requires an exact model alias and positive bounded dimensions")
	}
	for _, r := range model {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/:", r)) {
			return nil, errors.New("native embedding model alias contains unsupported characters")
		}
	}
	return &EmbeddingHandler{native: &nativeEmbeddingBinding{endpoint: endpoint, model: model, dimensions: dimensions}}, nil
}

func (h *EmbeddingHandler) embeddingOperation(req *EmbeddingRequest) (*teiEmbeddingOperation, error) {
	if h.native == nil {
		return newTEIEmbeddingOperation(teiBaseURL()), nil
	}
	if req.Model != h.native.model || req.Dimensions != h.native.dimensions {
		return nil, errors.New("embedding request does not match the native model and dimensions")
	}
	op := newTEIEmbeddingOperation(h.native.endpoint)
	op.native = h.native
	// The embedding input must stay on the bound local origin. Neither proxy
	// environment nor HTTP redirects may send it to another destination.
	op.client.Transport = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2}
	op.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return op, nil
}
