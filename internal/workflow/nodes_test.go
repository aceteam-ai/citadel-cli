package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInputNodeExecutor(t *testing.T) {
	e := &InputNodeExecutor{}
	node := &Node{ID: "in", Type: NodeTypeInput}
	output, err := e.Execute(context.Background(), node, map[string]any{"key": "value"})
	if err != nil || output["key"] != "value" {
		t.Fatal("expected passthrough")
	}
	output, _ = e.Execute(context.Background(), node, nil)
	if output == nil {
		t.Fatal("expected non-nil for nil input")
	}
}

func TestOutputNodeExecutor(t *testing.T) {
	e := &OutputNodeExecutor{}
	output, _ := e.Execute(context.Background(), &Node{ID: "out", Type: NodeTypeOutput}, map[string]any{"result": "data"})
	if output["result"] != "data" {
		t.Fatal("expected passthrough")
	}
}

func TestTransformNodeExecutor_Mapping(t *testing.T) {
	e := &TransformNodeExecutor{}
	node := &Node{ID: "t", Type: NodeTypeTransform, Params: map[string]any{
		"mapping": map[string]any{"output_field": "input_field"},
	}}
	output, _ := e.Execute(context.Background(), node, map[string]any{"input_field": "hello"})
	if output["output_field"] != "hello" {
		t.Fatalf("expected mapping, got %v", output)
	}
}

func TestTransformNodeExecutor_Template(t *testing.T) {
	e := &TransformNodeExecutor{}
	node := &Node{ID: "t", Type: NodeTypeTransform, Params: map[string]any{
		"template": map[string]any{"greeting": "Hello, {{name}}! You are {{age}} years old."},
	}}
	output, _ := e.Execute(context.Background(), node, map[string]any{"name": "Alice", "age": 30})
	if output["greeting"] != "Hello, Alice! You are 30 years old." {
		t.Fatalf("expected template expansion, got %v", output)
	}
}

func TestTransformNodeExecutor_Passthrough(t *testing.T) {
	e := &TransformNodeExecutor{}
	output, _ := e.Execute(context.Background(), &Node{ID: "t", Type: NodeTypeTransform, Params: map[string]any{}}, map[string]any{"a": 1})
	if output["a"] != 1 {
		t.Fatal("expected passthrough")
	}
}

func TestShellNodeExecutor(t *testing.T) {
	e := NewShellNodeExecutor(ShellConfig{})
	output, err := e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "echo hello"}}, nil)
	if err != nil || output["stdout"] != "hello" || output["exit_code"] != 0 {
		t.Fatalf("unexpected: %v %v", output, err)
	}
}

func TestShellNodeExecutor_DenyList(t *testing.T) {
	e := NewShellNodeExecutor(ShellConfig{DenyList: []string{"rm", "sudo"}})
	_, err := e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "rm -rf /"}}, nil)
	if err == nil {
		t.Fatal("expected error for denied command")
	}
}

func TestShellNodeExecutor_AllowList(t *testing.T) {
	e := NewShellNodeExecutor(ShellConfig{AllowList: []string{"echo", "cat"}})
	output, err := e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "echo ok"}}, nil)
	if err != nil || output["stdout"] != "ok" {
		t.Fatalf("unexpected: %v %v", output, err)
	}
	_, err = e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "ls -la"}}, nil)
	if err == nil {
		t.Fatal("expected error for disallowed command")
	}
}

func TestShellNodeExecutor_TemplateSubstitution(t *testing.T) {
	e := NewShellNodeExecutor(ShellConfig{})
	output, _ := e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "echo {{message}}"}}, map[string]any{"message": "world"})
	if output["stdout"] != "world" {
		t.Fatalf("expected 'world', got %q", output["stdout"])
	}
}

func TestShellNodeExecutor_NonZeroExit(t *testing.T) {
	e := NewShellNodeExecutor(ShellConfig{})
	output, err := e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "exit 42"}}, nil)
	if err != nil || output["exit_code"] != 42 {
		t.Fatalf("unexpected: %v %v", output, err)
	}
}

func TestHTTPNodeExecutor_GET(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	e := &HTTPNodeExecutor{}
	output, err := e.Execute(context.Background(), &Node{ID: "http", Type: NodeTypeHTTP, Params: map[string]any{"url": server.URL, "method": "GET"}}, nil)
	if err != nil || output["status_code"] != 200 {
		t.Fatalf("unexpected: %v %v", output, err)
	}
	jsonResp, ok := output["json"].(map[string]any)
	if !ok || jsonResp["status"] != "ok" {
		t.Fatal("expected parsed json")
	}
}

func TestHTTPNodeExecutor_POST(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"created": "true"})
	}))
	defer server.Close()
	e := &HTTPNodeExecutor{}
	output, _ := e.Execute(context.Background(), &Node{ID: "http", Type: NodeTypeHTTP, Params: map[string]any{"url": server.URL, "method": "POST"}}, map[string]any{"body": map[string]any{"key": "value"}})
	if output["status_code"] != 201 {
		t.Fatalf("expected 201, got %v", output["status_code"])
	}
}

func TestHTTPNodeExecutor_MissingURL(t *testing.T) {
	_, err := (&HTTPNodeExecutor{}).Execute(context.Background(), &Node{ID: "http", Type: NodeTypeHTTP}, nil)
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
}

func TestLLMNodeExecutor_MissingPrompt(t *testing.T) {
	_, err := (&LLMNodeExecutor{}).Execute(context.Background(), &Node{ID: "llm", Type: NodeTypeLLM, Params: map[string]any{"model": "llama3"}}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
}

func TestLLMNodeExecutor_WithMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": "Hello!"}})
	}))
	defer server.Close()
	output, err := (&LLMNodeExecutor{}).Execute(context.Background(), &Node{ID: "llm", Type: NodeTypeLLM, Params: map[string]any{"model": "llama3", "endpoint": server.URL}}, map[string]any{"prompt": "Say hi"})
	if err != nil || output["response"] != "Hello!" {
		t.Fatalf("unexpected: %v %v", output, err)
	}
}

func TestHelperFunctions(t *testing.T) {
	m := map[string]any{"s": "val", "i": 42, "f": 3.14}
	if stringParam(m, "s", "def") != "val" || stringParam(m, "x", "def") != "def" || stringParam(nil, "x", "def") != "def" {
		t.Fatal("stringParam broken")
	}
	if intParam(m, "i", 0) != 42 || intParam(m, "x", 99) != 99 {
		t.Fatal("intParam broken")
	}
	if floatParam(m, "f", 0) != 3.14 || floatParam(m, "x", 1.5) != 1.5 {
		t.Fatal("floatParam broken")
	}
}
