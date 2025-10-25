// internal/relay/server.go
package relay

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// corsMiddleware adds the necessary CORS headers to each response.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In a real production system, you might get this from a config.
		// For now, allowing any origin is fine for this local relay.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// newReverseProxy creates a new reverse proxy for a given target URL.
func newReverseProxy(target string) *httputil.ReverseProxy {
	url, err := url.Parse(target)
	if err != nil {
		log.Fatalf("Failed to parse target URL for proxy: %v", err)
	}
	return httputil.NewSingleHostReverseProxy(url)
}

// StartServer starts the Citadel Relay on localhost:31337.
func StartServer() {
	mux := http.NewServeMux()

	// Define the routes and their backend targets
	proxies := map[string]string{
		"/ollama/":   "http://localhost:11434",
		"/vllm/":     "http://localhost:8000",
		"/llamacpp/": "http://localhost:8080",
	}

	for prefix, target := range proxies {
		proxy := newReverseProxy(target)
		// The handler strips the prefix before forwarding.
		// e.g., /ollama/api/chat -> /api/chat
		mux.Handle(prefix, http.StripPrefix(prefix, proxy))
	}

	// Wrap the entire mux with our CORS middleware
	handler := corsMiddleware(mux)

	log.Println("   - 👂 Citadel Relay is listening on http://localhost:31337")
	if err := http.ListenAndServe(":31337", handler); err != nil {
		log.Printf("   - ⚠️  Citadel Relay failed to start: %v\n", err)
	}
}
