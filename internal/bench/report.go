package bench

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FormatReport formats a BenchmarkResult as a human-readable markdown table.
func FormatReport(r *BenchmarkResult) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("\nBenchmark: %s\n", r.Endpoint))
	sb.WriteString(fmt.Sprintf("Model: %s | Turns: %d | Concurrency: %d | Max Tokens: %d\n",
		r.Model, r.Turns, r.Concurrency, r.MaxTokens))
	sb.WriteString(fmt.Sprintf("Total Time: %s\n\n", formatDuration(r.TotalTime)))

	if r.Error != "" {
		sb.WriteString(fmt.Sprintf("Error: %s\n", r.Error))
		return sb.String()
	}

	// Summary
	sb.WriteString(fmt.Sprintf("  Avg Tokens/sec:  %.1f\n", r.AvgTokensPerSec))
	sb.WriteString(fmt.Sprintf("  Avg Latency:     %s\n", formatDuration(r.AvgLatency)))
	sb.WriteString(fmt.Sprintf("  Avg TTFT:        %s\n", formatDuration(r.AvgTTFT)))
	sb.WriteString(fmt.Sprintf("  Total Tokens:    %d\n", r.TotalTokens))

	// Per-turn table
	if len(r.TurnResults) > 1 {
		sb.WriteString("\n  Turn | Tokens/sec |   Latency |      TTFT | Tokens\n")
		sb.WriteString("  -----|------------|-----------|-----------|-------\n")
		for _, tr := range r.TurnResults {
			if tr.Error != "" {
				sb.WriteString(fmt.Sprintf("  %4d | ERROR: %s\n", tr.Turn, tr.Error))
				continue
			}
			sb.WriteString(fmt.Sprintf("  %4d | %10.1f | %9s | %9s | %5d\n",
				tr.Turn, tr.TokensPerSec,
				formatDuration(tr.Latency),
				formatDuration(tr.TTFT),
				tr.CompletionTokens))
		}
	}

	sb.WriteString("\n")
	return sb.String()
}

// FormatComparison formats a side-by-side comparison of two benchmark results.
func FormatComparison(a, b *BenchmarkResult) string {
	var sb strings.Builder

	sb.WriteString("\nComparison\n")
	sb.WriteString(strings.Repeat("=", 70) + "\n\n")

	// Header
	sb.WriteString(fmt.Sprintf("  %-20s | %-20s | %-20s\n", "Metric", truncate(a.Endpoint, 20), truncate(b.Endpoint, 20)))
	sb.WriteString(fmt.Sprintf("  %-20s | %-20s | %-20s\n", strings.Repeat("-", 20), strings.Repeat("-", 20), strings.Repeat("-", 20)))

	// Model
	sb.WriteString(fmt.Sprintf("  %-20s | %-20s | %-20s\n", "Model", truncate(a.Model, 20), truncate(b.Model, 20)))

	// Tokens/sec (higher is better)
	tokWinner := pickWinner(a.AvgTokensPerSec, b.AvgTokensPerSec, true)
	tokPct := percentDiff(a.AvgTokensPerSec, b.AvgTokensPerSec)
	sb.WriteString(fmt.Sprintf("  %-20s | %20.1f | %20.1f  %s\n",
		"Tokens/sec", a.AvgTokensPerSec, b.AvgTokensPerSec, tokWinner+" "+tokPct))

	// Latency (lower is better)
	latWinner := pickWinner(float64(a.AvgLatency), float64(b.AvgLatency), false)
	latPct := percentDiff(float64(a.AvgLatency), float64(b.AvgLatency))
	sb.WriteString(fmt.Sprintf("  %-20s | %20s | %20s  %s\n",
		"Avg Latency", formatDuration(a.AvgLatency), formatDuration(b.AvgLatency), latWinner+" "+latPct))

	// TTFT (lower is better)
	ttftWinner := pickWinner(float64(a.AvgTTFT), float64(b.AvgTTFT), false)
	ttftPct := percentDiff(float64(a.AvgTTFT), float64(b.AvgTTFT))
	sb.WriteString(fmt.Sprintf("  %-20s | %20s | %20s  %s\n",
		"Avg TTFT", formatDuration(a.AvgTTFT), formatDuration(b.AvgTTFT), ttftWinner+" "+ttftPct))

	// Total Tokens
	sb.WriteString(fmt.Sprintf("  %-20s | %20d | %20d\n",
		"Total Tokens", a.TotalTokens, b.TotalTokens))

	// Total Time
	sb.WriteString(fmt.Sprintf("  %-20s | %20s | %20s\n",
		"Total Time", formatDuration(a.TotalTime), formatDuration(b.TotalTime)))

	sb.WriteString("\n")
	return sb.String()
}

// FormatJSON returns the result(s) as pretty-printed JSON.
func FormatJSON(results ...*BenchmarkResult) (string, error) {
	var out any
	if len(results) == 1 {
		out = results[0]
	} else {
		out = results
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// pickWinner returns a label indicating which endpoint won for a metric.
// higherIsBetter controls the comparison direction.
func pickWinner(a, b float64, higherIsBetter bool) string {
	if a == 0 && b == 0 {
		return "  tie"
	}
	if higherIsBetter {
		if a > b {
			return "<< A wins"
		} else if b > a {
			return ">> B wins"
		}
	} else {
		if a < b {
			return "<< A wins"
		} else if b < a {
			return ">> B wins"
		}
	}
	return "  tie"
}

// percentDiff returns a human-readable percentage difference.
func percentDiff(a, b float64) string {
	if a == 0 || b == 0 {
		return ""
	}
	diff := ((a - b) / b) * 100
	if diff < 0 {
		diff = -diff
	}
	return fmt.Sprintf("(%.0f%%)", diff)
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// formatDuration formats a duration in human-friendly units.
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fus", float64(d.Microseconds()))
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
