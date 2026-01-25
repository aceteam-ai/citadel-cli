package tui

// AppConfig holds configuration for the TUI application
type AppConfig struct {
	Version  string
	Services []string
}

// Note: The actual RunInteractive function is in cmd/interactive.go
// to avoid import cycles between tui and tui/repl packages.
