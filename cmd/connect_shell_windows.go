//go:build windows

package cmd

import (
	"context"
	"os"
	"time"

	"golang.org/x/term"
)

// watchResize invokes onResize when the local terminal size changes. Windows
// has no SIGWINCH, so we poll the console size and fire onResize on a change.
func watchResize(ctx context.Context, onResize func()) {
	go func() {
		fd := int(os.Stdin.Fd())
		lastCols, lastRows, _ := term.GetSize(fd)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cols, rows, err := term.GetSize(fd)
				if err == nil && (cols != lastCols || rows != lastRows) {
					lastCols, lastRows = cols, rows
					onResize()
				}
			}
		}
	}()
}
