// internal/jobs/meeting_summary.go
//
// Synchronous, node-local vLLM summarization for the in-call `/ace summary`
// command (issue #5435, epic #5097). Unlike the Redis-job inference handlers
// (llm_inference.go / vllm_inference.go), this is a SMALL in-process helper
// called on the interactive meeting loop's own goroutine: a participant types
// `/ace summary`, the bot summarizes the live transcript buffer and posts the
// result back to Meet chat, all within one poll tick.
//
// DESIGN — why this does NOT reuse waitForVLLMReady:
//   - The Redis inference handlers poll vLLM's /health for up to 60s before
//     giving up. Doing that here would freeze the meeting loop (chat/leave/end
//     checks all pause) for a full minute whenever the model is cold or absent.
//     Instead this makes a SINGLE attempt under a tight context deadline
//     (localVLLMTimeout): any failure — unreachable, no served model, non-200,
//     empty completion, or timeout — returns an error so the caller posts a
//     graceful fallback message and the meeting continues untouched.
package jobs

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

// localVLLMBaseURL is the node-local vLLM OpenAI-compatible endpoint. It mirrors
// VLLMInferenceHandler (vllm_inference.go), which targets localhost:8000 for the
// meeting/inference node's vLLM. Kept as a single const so a port change is a
// one-place edit.
const localVLLMBaseURL = "http://localhost:8000"

// localVLLMTimeout bounds the WHOLE summary round-trip (model discovery + the
// completion). It is deliberately tight: the call runs on the meeting loop
// goroutine, so a long deadline would stall in-call chat/leave/end handling.
const localVLLMTimeout = 8 * time.Second

// meetingSummaryMaxTranscriptChars caps how much of the live transcript is fed
// to the model so a long call cannot blow the prompt (or the tight deadline).
// The TAIL is kept (recent discussion is the most summary-relevant) when the
// buffer exceeds the cap.
const meetingSummaryMaxTranscriptChars = 6000

// meetingSummarizer summarizes a transcript and returns a single-paragraph
// summary. It is injected into the command executor so tests can supply a fake
// (or a failing) implementation without a real vLLM. An error means "summary
// unavailable"; the caller posts a fallback message rather than propagating it.
type meetingSummarizer func(ctx context.Context, transcript string) (string, error)

// localVLLMSummarize is the production meetingSummarizer: it discovers the served
// model, then requests a short summary from the node-local vLLM. It never blocks
// longer than localVLLMTimeout and returns a plain error on any failure path so
// the meeting loop degrades to a fallback chat message.
func localVLLMSummarize(ctx context.Context, transcript string) (string, error) {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "", fmt.Errorf("empty transcript")
	}
	if len(transcript) > meetingSummaryMaxTranscriptChars {
		transcript = transcript[len(transcript)-meetingSummaryMaxTranscriptChars:]
	}

	cctx, cancel := context.WithTimeout(ctx, localVLLMTimeout)
	defer cancel()

	model, err := discoverLocalVLLMModel(cctx)
	if err != nil {
		return "", fmt.Errorf("discover vLLM model: %w", err)
	}

	prompt := buildMeetingSummaryPrompt(transcript)
	reqBody, _ := json.Marshal(map[string]any{
		"model":       model,
		"prompt":      prompt,
		"max_tokens":  256,
		"temperature": 0.3,
		"stop":        []string{"\n\n"},
	})

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, localVLLMBaseURL+"/v1/completions", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("connect to vLLM: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vLLM returned status %d", resp.StatusCode)
	}

	var decoded struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("parse vLLM response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", fmt.Errorf("vLLM returned no choices")
	}
	summary := strings.TrimSpace(decoded.Choices[0].Text)
	if summary == "" {
		return "", fmt.Errorf("vLLM returned an empty summary")
	}
	return summary, nil
}

// discoverLocalVLLMModel asks vLLM which model is served and returns the first
// id. vLLM validates the completion request's "model" against its served models,
// so the summary cannot hardcode a name — it must ask. Any failure (unreachable
// or no models) is an error that aborts the summary.
func discoverLocalVLLMModel(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localVLLMBaseURL+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("connect to vLLM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vLLM /v1/models returned status %d", resp.StatusCode)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("parse vLLM models: %w", err)
	}
	if len(decoded.Data) == 0 || strings.TrimSpace(decoded.Data[0].ID) == "" {
		return "", fmt.Errorf("vLLM reports no served models")
	}
	return decoded.Data[0].ID, nil
}

// buildMeetingSummaryPrompt frames the transcript for a completion model. The
// closing "Summary:" cue plus the "\n\n" stop keeps the output to one compact
// paragraph, which the caller collapses to a single chat line.
func buildMeetingSummaryPrompt(transcript string) string {
	return "You are a meeting notetaker. Summarize the meeting so far in 2-3 concise " +
		"sentences, capturing decisions and action items. Do not add anything not in the transcript.\n\n" +
		"Transcript:\n" + transcript + "\n\nSummary:"
}
