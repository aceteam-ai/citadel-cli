// cmd/bench.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/bench"
	"github.com/spf13/cobra"
)

var benchModel string
var benchMaxTokens int
var benchTurns int
var benchConcurrency string
var benchCompare string
var benchJSON bool

var benchCmd = &cobra.Command{
	Use:   "bench <endpoint_url>",
	Short: "Benchmark an OpenAI-compatible inference endpoint",
	Long: `Run performance benchmarks against OpenAI-compatible inference endpoints.

Measures tokens/sec, latency, and time-to-first-token (TTFT) using streaming
chat completions. Supports multi-turn conversations and concurrent requests.

The endpoint should serve the OpenAI-compatible /v1/chat/completions API
(vLLM, SGLang, Ollama, etc). The URL can be a base URL or the full path.`,
	Example: `  citadel bench http://localhost:8000
  citadel bench http://gpu-node:30000 --model qwen2-72b --max-tokens 200
  citadel bench http://localhost:8000 --turns 3 --concurrency 4
  citadel bench http://localhost:8000 --compare http://localhost:30000
  citadel bench http://localhost:8000 --json`,
	Args: cobra.ExactArgs(1),
	Run:  runBench,
}

func runBench(cmd *cobra.Command, args []string) {
	endpoint := args[0]

	concurrency, err := strconv.Atoi(benchConcurrency)
	if err != nil || concurrency < 1 {
		fmt.Fprintf(os.Stderr, "Error: --concurrency must be a positive integer, got %q\n", benchConcurrency)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Auto-detect model if not specified
	model := benchModel
	if model == "" {
		fmt.Printf("Auto-detecting model from %s...\n", endpoint)
		detected, err := bench.AutoDetectModel(ctx, endpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not auto-detect model: %v\nUse --model to specify explicitly.\n", err)
			os.Exit(1)
		}
		model = detected
		fmt.Printf("Detected model: %s\n", model)
	}

	// Run primary benchmark
	fmt.Printf("Benchmarking %s (model=%s, turns=%d, concurrency=%d, max_tokens=%d)...\n",
		endpoint, model, benchTurns, concurrency, benchMaxTokens)
	result := bench.RunBenchmark(ctx, endpoint, model, benchMaxTokens, benchTurns, concurrency)

	// If --compare is set, run second benchmark
	if benchCompare != "" {
		compareModel := model
		if benchModel == "" {
			// Auto-detect for compare endpoint too
			detected, err := bench.AutoDetectModel(ctx, benchCompare)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not auto-detect model for compare endpoint, using %s\n", model)
			} else {
				compareModel = detected
			}
		}

		fmt.Printf("\nBenchmarking %s (model=%s, turns=%d, concurrency=%d, max_tokens=%d)...\n",
			benchCompare, compareModel, benchTurns, concurrency, benchMaxTokens)
		compareResult := bench.RunBenchmark(ctx, benchCompare, compareModel, benchMaxTokens, benchTurns, concurrency)

		if benchJSON {
			output, err := bench.FormatJSON(result, compareResult)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(output)
		} else {
			fmt.Print(bench.FormatReport(result))
			fmt.Print(bench.FormatReport(compareResult))
			fmt.Print(bench.FormatComparison(result, compareResult))
		}
		return
	}

	// Single endpoint output
	if benchJSON {
		output, err := bench.FormatJSON(result)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error formatting JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(output)
	} else {
		fmt.Print(bench.FormatReport(result))
	}
}

func init() {
	rootCmd.AddCommand(benchCmd)
	benchCmd.Flags().StringVar(&benchModel, "model", "", "Model name (auto-detected from /v1/models if not set)")
	benchCmd.Flags().IntVar(&benchMaxTokens, "max-tokens", 50, "Maximum tokens to generate per turn")
	benchCmd.Flags().IntVar(&benchTurns, "turns", 1, "Number of conversation turns")
	benchCmd.Flags().StringVar(&benchConcurrency, "concurrency", "1", "Number of parallel requests")
	benchCmd.Flags().StringVar(&benchCompare, "compare", "", "Second endpoint URL for side-by-side comparison")
	benchCmd.Flags().BoolVar(&benchJSON, "json", false, "Output results as JSON")
}
