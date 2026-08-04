package memory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxTranscriptRead = int64(4 << 20)

type transcriptEntry struct {
	Type    string          `json:"type"`
	IsMeta  bool            `json:"isMeta"`
	Message json.RawMessage `json:"message"`
}

type transcriptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type transcriptBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// TranscriptSummary selects bounded human/assistant text from a Claude Code
// JSONL transcript. Tool payloads, thinking blocks, metadata, and malformed
// records are excluded; raw JSON is never copied into memory.
func TranscriptSummary(path string, budget int) string {
	if strings.TrimSpace(path) == "" || budget <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var reader io.Reader = f
	if info, statErr := f.Stat(); statErr == nil && info.Size() > maxTranscriptRead {
		if _, err := f.Seek(info.Size()-maxTranscriptRead, io.SeekStart); err != nil {
			return ""
		}
		// Discard the partial first JSONL record after seeking into the file.
		buffered := bufio.NewReader(f)
		_, _ = buffered.ReadString('\n')
		reader = buffered
	}

	var selected []string
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		role, text := semanticTranscriptLine(scanner.Bytes())
		if text == "" {
			continue
		}
		selected = append(selected, fmt.Sprintf("%s: %s", role, text))
		if len(selected) > 12 {
			selected = selected[len(selected)-12:]
		}
	}
	if len(selected) == 0 {
		return ""
	}

	// Keep the newest semantic messages that fit. If the newest message alone
	// exceeds the budget, retain its beginning at a rune boundary.
	var kept []string
	used := 0
	for i := len(selected) - 1; i >= 0; i-- {
		runes := []rune(selected[i])
		separator := 1
		if len(kept) == 0 {
			separator = 0
		}
		if used+separator+len(runes) > budget {
			if len(kept) == 0 {
				if budget > 1 {
					runes = append(runes[:budget-1], '…')
				} else {
					runes = runes[:budget]
				}
				kept = append(kept, string(runes))
			}
			break
		}
		kept = append([]string{selected[i]}, kept...)
		used += separator + len(runes)
	}
	return strings.Join(kept, "\n")
}

func semanticTranscriptLine(line []byte) (string, string) {
	var entry transcriptEntry
	if err := json.Unmarshal(line, &entry); err != nil || entry.IsMeta || len(entry.Message) == 0 {
		return "", ""
	}
	var msg transcriptMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		return "", ""
	}
	role := strings.ToLower(strings.TrimSpace(msg.Role))
	if role == "" {
		role = strings.ToLower(strings.TrimSpace(entry.Type))
	}
	label := ""
	switch role {
	case "user", "human":
		label = "User"
	case "assistant":
		label = "Assistant"
	default:
		return "", ""
	}

	var direct string
	if err := json.Unmarshal(msg.Content, &direct); err == nil {
		return label, normalizeTranscriptText(direct)
	}
	var blocks []transcriptBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return "", ""
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" {
			if text := normalizeTranscriptText(block.Text); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return label, strings.Join(texts, "\n")
}

func normalizeTranscriptText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// StableCaptureName returns a collision-resistant name that is stable across
// SessionEnd retries. The session id is preferred; transcript path and content
// are deterministic fallbacks. No identifier is exposed in the slug.
func StableCaptureName(sessionID, transcriptPath, content string) string {
	seed := "session:" + sessionID
	if sessionID == "" {
		seed = "transcript:" + transcriptPath
	}
	if sessionID == "" && transcriptPath == "" {
		seed = "content:" + content
	}
	sum := sha256.Sum256([]byte(seed))
	return "claude-session-" + hex.EncodeToString(sum[:10])
}
