//go:build !windows

package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// watchResize invokes onResize whenever the local terminal is resized, using
// the SIGWINCH signal (available on Unix-like platforms). It stops when ctx is
// cancelled.
func watchResize(ctx context.Context, onResize func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				onResize()
			}
		}
	}()
}
