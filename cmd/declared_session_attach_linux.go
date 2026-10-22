//go:build linux

package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func declaredTerminateSignal() os.Signal { return syscall.SIGTERM }

func runDeclaredAttach(cmd *cobra.Command, name string) error {
	input, inOK := cmd.InOrStdin().(*os.File)
	output, outOK := cmd.OutOrStdout().(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(input.Fd())) || !term.IsTerminal(int(output.Fd())) {
		return errors.New("named session attach requires a local terminal")
	}
	nodeDir, _ := cmd.Flags().GetString("node-config-dir")
	store, manager, err := openDeclaredManager(selectedDeclaredNodeDir(nodeDir))
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client, err := manager.Dial(ctx, name)
	if err != nil {
		return err
	}
	defer client.Close()
	cols, rows, err := declaredTerminalSize(int(input.Fd()))
	if err != nil {
		return err
	}
	readonly, _ := cmd.Flags().GetBool("readonly")
	takeControl, _ := cmd.Flags().GetBool("take-control")
	attachment, err := client.Attach(ctx, declaredsession.AttachOptions{ReadOnly: readonly, TakeControl: takeControl, Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	defer attachment.Close()
	spec, _ := cmd.Flags().GetString("detach-keys")
	decoder, err := newDeclaredDetachDecoder(spec)
	if err != nil {
		return err
	}
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return errors.New("cannot enter local terminal raw mode")
	}
	defer term.Restore(int(input.Fd()), state)
	// Use the captured descriptor numbers throughout. In particular, no
	// goroutine may later call File.Fd after close and accidentally use a
	// recycled descriptor. Restore status flags and terminal state on exit.
	inFD, outFD := int(input.Fd()), int(output.Fd())
	inFlags, err := unix.FcntlInt(uintptr(inFD), unix.F_GETFL, 0)
	if err != nil {
		return errors.New("cannot inspect local terminal flags")
	}
	outFlags, err := unix.FcntlInt(uintptr(outFD), unix.F_GETFL, 0)
	if err != nil {
		return errors.New("cannot inspect local terminal flags")
	}
	if _, err := unix.FcntlInt(uintptr(inFD), unix.F_SETFL, inFlags|unix.O_NONBLOCK); err != nil {
		return errors.New("cannot bound local terminal input")
	}
	defer unix.FcntlInt(uintptr(inFD), unix.F_SETFL, inFlags)
	if _, err := unix.FcntlInt(uintptr(outFD), unix.F_SETFL, outFlags|unix.O_NONBLOCK); err != nil {
		return errors.New("cannot bound local terminal output")
	}
	defer unix.FcntlInt(uintptr(outFD), unix.F_SETFL, outFlags)
	return declaredAttachLoop(ctx, attachment, decoder, inFD, outFD)
}

func declaredTerminalSize(fd int) (uint16, uint16, error) {
	cols, rows, err := term.GetSize(fd)
	if err != nil || cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return 0, 0, errors.New("local terminal dimensions unavailable or out of bounds")
	}
	return uint16(cols), uint16(rows), nil
}

type declaredAttachResult struct {
	event declaredsession.AttachEvent
	err   error
}

func declaredAttachLoop(parent context.Context, attachment *declaredsession.Attachment, decoder *declaredDetachDecoder, inFD, outFD int) error {
	ctx, cancel := context.WithCancel(parent)
	results := make(chan declaredAttachResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			event, err := attachment.Next(ctx)
			select {
			case results <- declaredAttachResult{event: event, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = attachment.Close() // Closes only this viewer's connection.
		<-done
	}()
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, syscall.SIGWINCH)
	defer signal.Stop(resizes)
	input := make([]byte, 8<<10)
	forward := func(data []byte) error {
		if len(data) == 0 || attachment.ReadOnly() {
			return nil
		}
		// A held prefix plus an entire new read can be one byte larger than
		// the wire input ceiling. Split only between frames, never drop it.
		for len(data) != 0 {
			n := len(data)
			if n > 8<<10 {
				n = 8 << 10
			}
			if err := attachment.Input(ctx, data[:n]); err != nil {
				if errors.Is(err, declaredsession.ErrRole) {
					return nil
				}
				return err
			}
			data = data[n:]
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resizes:
			if !attachment.ReadOnly() {
				cols, rows, err := declaredTerminalSize(inFD)
				if err != nil {
					return err
				}
				if err := attachment.Resize(ctx, cols, rows); err != nil && !errors.Is(err, declaredsession.ErrRole) {
					return err
				}
			}
		case result := <-results:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					return nil
				}
				return result.err
			}
			if result.event.Kind == "stopped" {
				return nil
			}
			if result.event.Kind == "role" && !result.event.ReadOnly {
				cols, rows, err := declaredTerminalSize(inFD)
				if err != nil {
					return err
				}
				if err := attachment.Resize(ctx, cols, rows); err != nil && !errors.Is(err, declaredsession.ErrRole) {
					return err
				}
			}
			if result.event.Kind == "output" {
				if err := writeDeclaredTerminal(ctx, outFD, result.event.Data); err != nil {
					return err
				}
			}
		default:
		}
		// Bounded polling avoids a stdin goroutine stranded in a blocking read
		// after detach. Read-only viewers still consume the local detach chord.
		fds := []unix.PollFd{{Fd: int32(inFD), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 20)
		if err == unix.EINTR {
			continue
		}
		if err != nil || fds[0].Revents&unix.POLLNVAL != 0 {
			return errors.New("local terminal input failed")
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
			continue
		}
		n, err := unix.Read(inFD, input)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return errors.New("local terminal input failed")
		}
		if n == 0 {
			return forward(decoder.Flush())
		}
		data, detach := decoder.Feed(input[:n])
		if err := forward(data); err != nil {
			return err
		}
		if detach {
			return nil
		}
	}
}

func writeDeclaredTerminal(ctx context.Context, fd int, data []byte) error {
	deadline := time.Now().Add(5 * time.Second)
	for len(data) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("slow local terminal detached")
		}
		n, err := unix.Write(fd, data)
		if n > 0 {
			data = data[n:]
		}
		if err == unix.EINTR {
			continue
		}
		if err == unix.EAGAIN || (err == nil && n == 0) {
			if _, pollErr := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}, 20); pollErr != nil && pollErr != unix.EINTR {
				return errors.New("local terminal output failed")
			}
			continue
		}
		if err != nil {
			return errors.New("local terminal output failed")
		}
	}
	return nil
}
