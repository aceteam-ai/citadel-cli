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
	"time"
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

type LLMNodeExecutor struct{}

func (e *LLMNodeExecutor) Execute(ctx context.Context, node *Node, input map[string]any) (map[string]any, error) {
	model := stringParam(node.Params, "model", "llama3")
	prompt := stringParam(input, "prompt", "")
	systemPrompt := stringParam(node.Params, "system_prompt", "")
	temperature := floatParam(node.Params, "temperature", 0.7)
	maxTokens := intParam(node.Params, "max_tokens", 2048)
	if prompt == "" {
		return nil, fmt.Errorf("LLM node %q: missing 'prompt' input", node.ID)
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
		return nil, fmt.Errorf("LLM node %q: marshal request: %w", node.ID, err)
	}
	endpoint := stringParam(node.Params, "endpoint", "http://127.0.0.1:11434/api/chat")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("LLM node %q: create request: %w", node.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LLM node %q: request failed: %w", node.ID, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("LLM node %q: read response: %w", node.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM node %q: server returned %d: %s", node.ID, resp.StatusCode, string(respBody))
	}
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("LLM node %q: parse response: %w", node.ID, err)
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
	command := stringParam(node.Params, "command", "")
	if command == "" {
		command = stringParam(input, "command", "")
	}
	if command == "" {
		return nil, fmt.Errorf("Shell node %q: missing 'command' param or input", node.ID)
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
	for k, v := range input {
		command = strings.ReplaceAll(command, "{{"+k+"}}", fmt.Sprint(v))
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

type HTTPNodeExecutor struct{}

func (e *HTTPNodeExecutor) Execute(ctx context.Context, node *Node, input map[string]any) (map[string]any, error) {
	urlStr := stringParam(node.Params, "url", "")
	if urlStr == "" {
		urlStr = stringParam(input, "url", "")
	}
	if urlStr == "" {
		return nil, fmt.Errorf("HTTP node %q: missing 'url' param or input", node.ID)
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
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
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
	timeout := time.Duration(intParam(node.Params, "timeout", 30)) * time.Second
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP node %q: request failed: %w", node.ID, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("HTTP node %q: read response: %w", node.ID, err)
	}
	result := map[string]any{"status_code": resp.StatusCode, "body": string(respBody)}
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
