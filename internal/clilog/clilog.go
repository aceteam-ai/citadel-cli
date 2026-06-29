// Package clilog provides citadel-cli's always-on, date-based, append-only
// file logging. It is a small dependency-free sink shared by the cobra
// commands (package cmd) and the control-center TUI (package
// internal/tui/controlcenter) so that every activity entry is persisted to
// disk as it happens — not only when the operator presses a key.
//
// Logs live in ~/.citadel-cli/logs/citadel-YYYY-MM-DD.log. All sessions on a
// given day append to the same file (one file per date, not one per process
// start), and a `latest.log` symlink always points at the current day's file.
// The file rotates automatically when the date rolls over for a long-running
// daemon.
package clilog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	mu         sync.Mutex
	file       *os.File
	fileDate   string // YYYY-MM-DD of the currently open file
	headerOnce sync.Once

	// dirOverride lets tests redirect the log directory. When empty, the
	// real ~/.citadel-cli/logs directory is used.
	dirOverride string
	// nowFn is the clock, overridable in tests.
	nowFn = time.Now
)

const dateLayout = "2006-01-02"

// Dir returns the directory where citadel logs are written.
func Dir() string {
	if dirOverride != "" {
		return dirOverride
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".citadel-cli", "logs")
}

// fileNameFor returns the log filename for a given day.
func fileNameFor(t time.Time) string {
	return fmt.Sprintf("citadel-%s.log", t.Format(dateLayout))
}

// Path returns the absolute path of the current day's log file.
func Path() string {
	return filepath.Join(Dir(), fileNameFor(nowFn()))
}

// ensureFileLocked opens (or rotates to) the log file for the current date.
// The caller must hold mu. It is best-effort: on any error the file handle is
// left nil and writes become no-ops rather than crashing the process.
func ensureFileLocked() {
	today := nowFn().Format(dateLayout)
	if file != nil && fileDate == today {
		return
	}

	d := Dir()
	if d == "" {
		return
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return
	}

	if file != nil {
		_ = file.Close()
		file = nil
	}

	name := fileNameFor(nowFn())
	f, err := os.OpenFile(filepath.Join(d, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	file = f
	fileDate = today

	// Point latest.log at today's file (relative target for portability).
	latest := filepath.Join(d, "latest.log")
	_ = os.Remove(latest)
	_ = os.Symlink(name, latest)
}

// Write appends a single timestamped line to today's log file. A level of ""
// is written under the generic [CITADEL] tag; otherwise the level (e.g.
// "warning") is recorded so the on-disk log mirrors the TUI activity panel.
func Write(level, msg string) {
	mu.Lock()
	defer mu.Unlock()

	ensureFileLocked()
	if file == nil {
		return
	}

	// One session header per process, written via whichever path logs first.
	headerOnce.Do(func() {
		fmt.Fprintf(file, "[%s] [CITADEL] === Session started ===\n", nowFn().Format("15:04:05"))
	})

	ts := nowFn().Format("15:04:05")
	if level == "" {
		fmt.Fprintf(file, "[%s] [CITADEL] %s\n", ts, msg)
	} else {
		fmt.Fprintf(file, "[%s] [%s] %s\n", ts, level, msg)
	}
}

// Writef is a printf-style convenience wrapper around Write.
func Writef(level, format string, args ...interface{}) {
	Write(level, fmt.Sprintf(format, args...))
}
