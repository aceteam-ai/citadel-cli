// services/ollama_keepalive.go
//
// aceteam-ai/citadel-cli#1560: manifest `ollama_keep_alive:` -> OLLAMA_KEEP_ALIVE
// and `ollama_preload:` model warm-up. A cold load of a few-GB model can take
// ~90s, longer than the platform chat timeout, and ollama unloads after its
// default 5 minute keep_alive. Mirrors ollama_policy.go (#1209): a dedicated
// typed manifest field normalized into the compose process env by one function
// on every compose-up path; the compose files declare the key-only form
// `environment: [OLLAMA_KEEP_ALIVE]`. Like #1209, unset becomes an explicit
// empty value.
package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EnvOllamaKeepAlive is the env var the ollama image reads for its default
// model keep_alive.
const EnvOllamaKeepAlive = "OLLAMA_KEEP_ALIVE"

// ValidateOllamaKeepAlive accepts "-1" (never unload) or a positive Go duration
// such as "30m" or "24h". Everything else (bare numbers, "0", other negatives,
// empty) is refused.
func ValidateOllamaKeepAlive(v string) error {
	if v == "-1" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fmt.Errorf("invalid ollama_keep_alive %q: must be -1 or a positive duration like 30m or 24h", v)
	}
	return nil
}

// OllamaKeepAliveComposeEnv returns env with exactly one OLLAMA_KEEP_ALIVE
// entry at the end for the ollama service: the manifest value when valid, else
// an explicit empty value (unset or invalid; invalid also returns an error).
// The empty value mirrors #1209 and shields the key-only compose passthrough
// from an ambient shell or .env OLLAMA_KEEP_ALIVE. Other services get env
// unchanged. It does not mutate env.
func OllamaKeepAliveComposeEnv(env []string, service string, keepAlive *string) ([]string, error) {
	if service != "ollama" {
		return env, nil
	}
	value := ""
	var err error
	if keepAlive != nil {
		if err = ValidateOllamaKeepAlive(*keepAlive); err == nil {
			value = *keepAlive
		}
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != EnvOllamaKeepAlive {
			out = append(out, entry)
		}
	}
	return append(out, EnvOllamaKeepAlive+"="+value), err
}

// Preload bounds.
const (
	ollamaPreloadReadyTimeout = 3 * time.Minute
	ollamaPreloadModelTimeout = 10 * time.Minute
)

// OllamaPreloadURL is the default local ollama endpoint.
const OllamaPreloadURL = "http://127.0.0.1:11434"

// OllamaKeepAliveJSON renders a validated keep_alive for an ollama request body:
// -1 must be a JSON number (the string "-1" fails ollama's duration parse).
func OllamaKeepAliveJSON(v string) any {
	if v == "-1" {
		return -1
	}
	return v
}

// PreloadOllamaModels waits for ollama at baseURL to answer /api/tags, then asks
// it to load each model once with keep_alive (omitted when keepAlive is empty).
// Best effort: failures are logged via logf and never returned; the whole call
// is bounded. Intended to run in a goroutine after a successful start.
func PreloadOllamaModels(ctx context.Context, client *http.Client, baseURL string, models []string, keepAlive string, logf func(string, ...any)) {
	if len(models) == 0 {
		return
	}
	if client == nil {
		client = http.DefaultClient
	}
	readyCtx, cancel := context.WithTimeout(ctx, ollamaPreloadReadyTimeout)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(readyCtx, http.MethodGet, baseURL+"/api/tags", nil)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-readyCtx.Done():
			logf("ollama preload: ollama not ready: %v", readyCtx.Err())
			return
		case <-time.After(2 * time.Second):
		}
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		body := map[string]any{"model": model, "stream": false}
		if keepAlive != "" {
			body["keep_alive"] = OllamaKeepAliveJSON(keepAlive)
		}
		raw, _ := json.Marshal(body)
		mctx, mcancel := context.WithTimeout(ctx, ollamaPreloadModelTimeout)
		req, _ := http.NewRequestWithContext(mctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			logf("ollama preload: %s: %v", model, err)
			mcancel()
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		mcancel()
		if resp.StatusCode != http.StatusOK {
			logf("ollama preload: %s: HTTP %d", model, resp.StatusCode)
			continue
		}
		logf("ollama preload: %s resident", model)
	}
}
