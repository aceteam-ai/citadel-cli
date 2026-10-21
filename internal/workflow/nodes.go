package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type NodeExecutor interface {
	Execute(ctx context.Context, node *Node, input map[string]any) (map[string]any, error)
}

type InputNodeExecutor struct{}

func (e *InputNodeExecutor) Execute(_ context.Context, _ *Node, input map[string]any) (map[string]any, error) {
	if input == nil {
		return make(map[string]any), nil
	}
	return input, nil
}

type OutputNodeExecutor struct{}

func (e *OutputNodeExecutor) Execute(_ context.Context, _ *Node, input map[string]any) (map[string]any, error) {
	if input == nil {
		return make(map[string]any), nil
	}
	return input, nil
}

type LLMNodeExecutor struct {
	policy LLMPolicy
}

func (e *LLMNodeExecutor) Execute(ctx context.Context, node *Node, input map[string]any) (map[string]any, error) {
	model := stringParam(node.Params, "model", "llama3")
	prompt := stringParam(input, "prompt", "")
	systemPrompt := stringParam(node.Params, "system_prompt", "")
	temperature := floatParam(node.Params, "temperature", 0.7)
	maxTokens := intParam(node.Params, "max_tokens", 2048)
	if prompt == "" {
		return nil, fmt.Errorf("LLM node %q: missing 'prompt' input", node.ID)
	}
	requestCtx, cancel := context.WithTimeout(ctx, llmRequestTimeout)
	defer cancel()
	endpoint := stringParam(node.Params, "endpoint", "http://127.0.0.1:11434/api/chat")
	validatedURL, pinnedIP, err := e.policy.resolveURL(requestCtx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("LLM node %q: %w", node.ID, err)
	}
	messages := []map[string]string{}
	if systemPrompt != "" {
		messages = append(messages, map[string]string{"role": "system", "content": systemPrompt})
	}
	messages = append(messages, map[string]string{"role": "user", "content": prompt})
	reqBody := map[string]any{
		"model": model, "messages": messages, "stream": false,
		"options": map[string]any{"temperature": temperature, "num_predict": maxTokens},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("LLM node %q: invalid request", node.ID)
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, validatedURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("LLM node %q: invalid request", node.ID)
	}
	req.Header.Set("Content-Type", "application/json")
	client := e.policy.clientFor(validatedURL, pinnedIP)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if err := requestCtx.Err(); err != nil {
			return nil, fmt.Errorf("LLM node %q: %w", node.ID, err)
		}
		return nil, fmt.Errorf("LLM node %q: request failed", node.ID)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM node %q: server returned HTTP %d", node.ID, resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes+1))
	if err != nil {
		if err := requestCtx.Err(); err != nil {
			return nil, fmt.Errorf("LLM node %q: %w", node.ID, err)
		}
		return nil, fmt.Errorf("LLM node %q: read response failed", node.ID)
	}
	if len(respBody) > maxLLMResponseBytes {
		return nil, fmt.Errorf("LLM node %q: %w", node.ID, ErrLLMResponseTooLarge)
	}
	if err := requestCtx.Err(); err != nil {
		return nil, fmt.Errorf("LLM node %q: %w", node.ID, err)
	}
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("LLM node %q: invalid response", node.ID)
	}
	content := ""
	if msg, ok := result["message"].(map[string]any); ok {
		if c, ok := msg["content"].(string); ok {
			content = c
		}
	}
	return map[string]any{"response": content, "model": model, "raw": result}, nil
}

type ShellNodeExecutor struct {
	config ShellConfig
}

func NewShellNodeExecutor(cfg ShellConfig) *ShellNodeExecutor {
	return &ShellNodeExecutor{config: cfg}
}

func (e *ShellNodeExecutor) Execute(ctx context.Context, node *Node, input map[string]any) (map[string]any, error) {
	// The run must have passed the shell gate at Submit (permission + passcode),
	// and the live permission is re-read here so revoking Shell stops a run's
	// remaining Shell nodes. Both refuse before any command is built.
	if !isShellAuthorized(ctx) {
		return nil, fmt.Errorf("Shell node %q: run did not pass the node shell gate", node.ID)
	}
	if !e.config.Gate.IsEnabled() {
		return nil, jobs.WorkflowShellRefusal(jobs.ReasonShellDisabled)
	}
	command := stringParam(node.Params, "command", "")
	if command == "" {
		command = stringParam(input, "command", "")
	}
	if command == "" {
		return nil, fmt.Errorf("Shell node %q: missing 'command' param or input", node.ID)
	}
	// Substitute inputs BEFORE the deny/allow checks: the checks must judge the
	// command that will actually run, or an input value could carry a denied
	// pattern or a second command past them.
	for k, v := range input {
		command = strings.ReplaceAll(command, "{{"+k+"}}", fmt.Sprint(v))
	}
	for _, denied := range e.config.DenyList {
		if strings.Contains(command, denied) {
			return nil, fmt.Errorf("Shell node %q: command contains denied pattern %q", node.ID, denied)
		}
	}
	if len(e.config.AllowList) > 0 {
		allowed := false
		for _, a := range e.config.AllowList {
			if strings.HasPrefix(strings.TrimSpace(command), a) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("Shell node %q: command not in allow list", node.ID)
		}
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if e.config.WorkspaceDir != "" {
		cmd.Dir = e.config.WorkspaceDir
	}
	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("Shell node %q: exec error: %w", node.ID, err)
		}
	}
	return map[string]any{"stdout": strings.TrimRight(string(output), "\n"), "exit_code": exitCode}, nil
}

type HTTPNodeExecutor struct {
	policy HTTPPolicy
}

func (e *HTTPNodeExecutor) Execute(ctx context.Context, node *Node, input map[string]any) (map[string]any, error) {
	urlStr := stringParam(node.Params, "url", "")
	if urlStr == "" {
		urlStr = stringParam(input, "url", "")
	}
	if urlStr == "" {
		return nil, fmt.Errorf("HTTP node %q: missing 'url' param or input", node.ID)
	}
	timeout := workflowHTTPTimeout(intParam(node.Params, "timeout", 30))
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	validatedURL, pinnedIP, err := e.policy.resolveURL(requestCtx, urlStr)
	if err != nil {
		return nil, fmt.Errorf("HTTP node %q: %w", node.ID, err)
	}
	method := strings.ToUpper(stringParam(node.Params, "method", "GET"))
	var bodyReader io.Reader
	if method == "POST" || method == "PUT" || method == "PATCH" {
		if bodyStr, ok := input["body"].(string); ok {
			bodyReader = strings.NewReader(bodyStr)
		} else if bodyMap, ok := input["body"].(map[string]any); ok {
			b, _ := json.Marshal(bodyMap)
			bodyReader = bytes.NewReader(b)
		} else if bodyParam := stringParam(node.Params, "body", ""); bodyParam != "" {
			bodyReader = strings.NewReader(bodyParam)
		}
	}
	req, err := http.NewRequestWithContext(requestCtx, method, validatedURL.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("HTTP node %q: create request: %w", node.ID, err)
	}
	if headers, ok := node.Params["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}
	if req.Header.Get("Content-Type") == "" && bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := e.policy.clientFor(validatedURL, pinnedIP, timeout)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP node %q: request failed: %w", node.ID, safeHTTPTransportError(err))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("HTTP node %q: read response: %w", node.ID, safeHTTPTransportError(err))
	}
	result := map[string]any{"status_code": resp.StatusCode, "body": string(respBody)}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
		result["location"] = resp.Header.Get("Location")
	}
	var jsonResp any
	if err := json.Unmarshal(respBody, &jsonResp); err == nil {
		result["json"] = jsonResp
	}
	return result, nil
}

type TransformNodeExecutor struct{}

func (e *TransformNodeExecutor) Execute(_ context.Context, node *Node, input map[string]any) (map[string]any, error) {
	output := make(map[string]any)
	if mapping, ok := node.Params["mapping"].(map[string]any); ok {
		for outKey, inKeyRaw := range mapping {
			inKey, ok := inKeyRaw.(string)
			if !ok {
				continue
			}
			if val, exists := input[inKey]; exists {
				output[outKey] = val
			}
		}
		return output, nil
	}
	if tmpl, ok := node.Params["template"].(map[string]any); ok {
		for outKey, patternRaw := range tmpl {
			pattern, ok := patternRaw.(string)
			if !ok {
				continue
			}
			result := pattern
			for k, v := range input {
				result = strings.ReplaceAll(result, "{{"+k+"}}", fmt.Sprint(v))
			}
			output[outKey] = result
		}
		return output, nil
	}
	for k, v := range input {
		output[k] = v
	}
	return output, nil
}

func stringParam(m map[string]any, key, defaultVal string) string {
	if m == nil {
		return defaultVal
	}
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return defaultVal
}

func intParam(m map[string]any, key string, defaultVal int) int {
	if m == nil {
		return defaultVal
	}
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	}
	return defaultVal
}

func floatParam(m map[string]any, key string, defaultVal float64) float64 {
	if m == nil {
		return defaultVal
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return defaultVal
}
