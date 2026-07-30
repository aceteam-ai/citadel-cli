package jobs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatMessageMultimodal covers the #625 change: ChatMessage.Content is raw
// JSON so it carries either a plain string or the OpenAI multimodal content-parts
// array (needed for vision/OCR models like baidu/Unlimited-OCR).
func TestChatMessageMultimodal(t *testing.T) {
	t.Run("string content unmarshals and Text returns it", func(t *testing.T) {
		var m ChatMessage
		if err := json.Unmarshal([]byte(`{"role":"user","content":"hello world"}`), &m); err != nil {
			t.Fatalf("unmarshal string content: %v", err)
		}
		if got := m.Text(); got != "hello world" {
			t.Errorf("Text() = %q, want %q", got, "hello world")
		}
	})

	t.Run("multimodal array unmarshals (was a hard error before #625)", func(t *testing.T) {
		raw := `{"role":"user","content":[{"type":"text","text":"<image>document parsing."},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}`
		var m ChatMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("unmarshal multimodal content: %v", err)
		}
		if got := m.Text(); got != "<image>document parsing." {
			t.Errorf("Text() = %q, want the concatenated text parts", got)
		}
	})

	t.Run("empty content is safe", func(t *testing.T) {
		var m ChatMessage
		if m.Text() != "" {
			t.Errorf("Text() on empty = %q, want empty", m.Text())
		}
		if string(m.ContentJSON()) != `""` {
			t.Errorf("ContentJSON() on empty = %s, want empty JSON string", m.ContentJSON())
		}
	})

	t.Run("ContentJSON forwards image parts verbatim into an outgoing request", func(t *testing.T) {
		raw := `{"role":"user","content":[{"type":"text","text":"t"},{"type":"image_url","image_url":{"url":"data:image/png;base64,ZZ"}}]}`
		var m ChatMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		// Mirror executeChatCompletionsAt's forwarding shape.
		out := map[string]any{"role": m.Role, "content": m.ContentJSON()}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal forwarded message: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, `"image_url"`) || !strings.Contains(s, "data:image/png;base64,ZZ") {
			t.Errorf("forwarded payload dropped the image part: %s", s)
		}
	})

	t.Run("string content round-trips through the forward path as a string", func(t *testing.T) {
		var m ChatMessage
		if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"content": m.ContentJSON()})
		if string(b) != `{"content":"hi"}` {
			t.Errorf("string content forward = %s, want {\"content\":\"hi\"}", b)
		}
	})
}
