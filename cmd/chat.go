// cmd/chat.go
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/localchat"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	chatEngine    string
	chatModel     string
	chatMaxTokens int
	chatNoReason  bool
)

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Chat with a model hosted locally on this node",
	Long: `Chat with an AI model served locally on THIS node.

Discovers the inference engine(s) running on this machine (vLLM, Ollama,
llama.cpp, or Bonsai), lets you pick a model if more than one is available,
and opens an interactive streaming chat against its local API.

Thinking models (e.g. Bonsai) stream their reasoning separately from the
answer; the reasoning is shown dimmed and the answer in normal text.

Type your message and press Enter. Ctrl-C interrupts a streaming reply;
type /exit (or Ctrl-D) to quit.`,
	// A runtime failure here (no engine running, engine unreachable) is an
	// operational condition, not misuse — don't dump the usage text on it.
	SilenceUsage: true,
	RunE:         runChat,
}

func init() {
	chatCmd.Flags().StringVar(&chatEngine, "engine", "", "Pre-select an engine by name (vllm, ollama, llamacpp, bonsai)")
	chatCmd.Flags().StringVar(&chatModel, "model", "", "Pre-select a model by id")
	chatCmd.Flags().IntVar(&chatMaxTokens, "max-tokens", localchat.DefaultMaxTokens, "Max tokens to generate per reply")
	chatCmd.Flags().BoolVar(&chatNoReason, "no-reasoning", false, "Hide the model's chain-of-thought (thinking models only)")
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Discover engines running on this node and flatten to (engine, model) choices.
	engines := status.DiscoverLocalEngines(ctx)
	choices := localchat.BuildChoices(engines)
	if len(choices) == 0 {
		return errors.New("no inference engine is running on this node.\n" +
			"Start one first, e.g.: citadel run --service bonsai  (or vllm / ollama / llamacpp)")
	}

	// Apply optional --engine / --model filters.
	choices = filterChoices(choices, chatEngine, chatModel)
	if len(choices) == 0 {
		return fmt.Errorf("no running engine/model matched --engine=%q --model=%q", chatEngine, chatModel)
	}

	choice, err := pickChoice(choices)
	if err != nil {
		return err
	}

	client := localchat.NewClient(choice.Port, choice.Model)
	if err := client.HealthCheck(ctx); err != nil {
		return fmt.Errorf("engine %q is not responding on localhost:%d: %w", choice.Engine, choice.Port, err)
	}

	return chatLoop(ctx, client, choice)
}

// filterChoices narrows choices by an optional engine and/or model name.
// Empty filters match everything; matching is case-insensitive.
func filterChoices(choices []localchat.EngineChoice, engine, model string) []localchat.EngineChoice {
	if engine == "" && model == "" {
		return choices
	}
	var out []localchat.EngineChoice
	for _, c := range choices {
		if engine != "" && !strings.EqualFold(c.Engine, engine) {
			continue
		}
		if model != "" && !strings.EqualFold(c.Model, model) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// pickChoice auto-selects when there is exactly one choice, otherwise prompts.
func pickChoice(choices []localchat.EngineChoice) (localchat.EngineChoice, error) {
	if len(choices) == 1 {
		return choices[0], nil
	}
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.Label()
	}
	selected, err := ui.AskSelect("Multiple models are available. Choose one to chat with:", labels)
	if err != nil {
		return localchat.EngineChoice{}, err
	}
	for i, l := range labels {
		if l == selected {
			return choices[i], nil
		}
	}
	return localchat.EngineChoice{}, errors.New("no model selected")
}

// chatLoop runs the interactive multi-turn conversation.
func chatLoop(ctx context.Context, client *localchat.Client, choice localchat.EngineChoice) error {
	promptStyle := color.New(color.FgCyan, color.Bold)
	assistantStyle := color.New(color.FgGreen, color.Bold)
	reasonStyle := color.New(color.FgHiBlack)
	mutedStyle := color.New(color.FgHiBlack)

	label := choice.Model
	if label == "" {
		label = choice.Engine + " (default model)"
	}
	assistantStyle.Printf("Chatting with %s", label)
	fmt.Printf(" on localhost:%d\n", choice.Port)
	mutedStyle.Println("Type your message and press Enter. Ctrl-C interrupts a reply; /exit or Ctrl-D quits.")
	fmt.Println()

	// Interrupt handling: Ctrl-C cancels an in-flight reply; at the prompt it
	// exits gracefully. A shared flag + cancel func distinguishes the two.
	var (
		mu          sync.Mutex
		streaming   bool
		cancelReply context.CancelFunc
	)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			mu.Lock()
			if streaming && cancelReply != nil {
				cancelReply()
				mu.Unlock()
				continue
			}
			mu.Unlock()
			mutedStyle.Println("\nGoodbye.")
			os.Exit(0)
		}
	}()

	messages := []localchat.Message{}
	reader := bufio.NewReader(os.Stdin)

	for {
		promptStyle.Print("you> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				mutedStyle.Println("\nGoodbye.")
				return nil
			}
			return err
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		switch strings.ToLower(input) {
		case "/exit", "/quit", "exit", "quit", ":q":
			mutedStyle.Println("Goodbye.")
			return nil
		}

		messages = append(messages, localchat.Message{Role: "user", Content: input})

		reqCtx, cancel := context.WithCancel(ctx)
		mu.Lock()
		streaming = true
		cancelReply = cancel
		mu.Unlock()

		assistantStyle.Print("bot> ")

		var answer strings.Builder
		inReasoning := false
		answerStarted := false
		streamErr := client.Stream(reqCtx, messages, chatMaxTokens, func(ch localchat.StreamChunk) {
			if ch.Reasoning != "" && !chatNoReason {
				if !inReasoning {
					reasonStyle.Print("(thinking) ")
					inReasoning = true
				}
				reasonStyle.Print(ch.Reasoning)
			}
			if ch.Content != "" {
				if inReasoning {
					// Separate the dimmed reasoning from the answer.
					fmt.Print("\n\n")
					inReasoning = false
				}
				answerStarted = true
				fmt.Print(ch.Content)
				answer.WriteString(ch.Content)
			}
		})

		mu.Lock()
		streaming = false
		cancelReply = nil
		mu.Unlock()
		cancel()
		fmt.Println()

		if streamErr != nil {
			if errors.Is(streamErr, context.Canceled) || reqCtx.Err() != nil {
				mutedStyle.Println("[interrupted]")
			} else {
				color.New(color.FgRed).Printf("error: %v\n", streamErr)
				// Drop the unanswered user turn so a retry isn't polluted.
				messages = messages[:len(messages)-1]
				continue
			}
		}

		// Persist ONLY the answer content in history — never reasoning_content
		// (per-turn scratch that degrades a thinking model if resent).
		if answerStarted && answer.Len() > 0 {
			messages = append(messages, localchat.Message{Role: "assistant", Content: answer.String()})
		} else {
			// No usable answer (interrupted before any content): drop the user turn.
			messages = messages[:len(messages)-1]
		}
	}
}
