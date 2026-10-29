//go:build linux

package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func declaredTerminateSignal() os.Signal { return syscall.SIGTERM }

func runDeclaredAttach(cmd *cobra.Command, name string) error {
	nodeDir, _ := cmd.Flags().GetString("node-config-dir")
	selected, err := selectedDeclaredNodeDir(cmd, nodeDir)
	if err != nil {
		return err
	}
	input, inOK := cmd.InOrStdin().(*os.File)
	output, outOK := cmd.OutOrStdout().(*os.File)
	if !inOK || !outOK {
		return errors.New("named session attach requires a local terminal")
	}
	inFD, err := openPrivateDeclaredTerminal(input, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer unix.Close(inFD)
	outFD, err := openPrivateDeclaredTerminal(output, unix.O_WRONLY)
	if err != nil {
		return err
	}
	defer unix.Close(outFD)
	store, manager, err := openDeclaredManager(selected)
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
	cols, rows, err := declaredTerminalSize(inFD)
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
	state, err := term.MakeRaw(inFD)
	if err != nil {
		return errors.New("cannot enter local terminal raw mode")
	}
	defer term.Restore(inFD, state)
	return declaredAttachLoop(ctx, attachment, decoder, inFD, outFD)
}

// Reopening through procfs creates independent open-file descriptions. Use
// SyscallConn rather than File.Fd, which can change a Go-managed file's flags.
// Never set O_NONBLOCK on inherited/shared terminal descriptions: a killed
// client cannot run defers to restore shared flags for the shell.
func openPrivateDeclaredTerminal(file *os.File, direction int) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return -1, errors.New("cannot inspect local terminal")
	}
	private := -1
	var openErr error
	err = raw.Control(func(fd uintptr) {
		private, openErr = privateDeclaredTerminal(int(fd), direction)
	})
	if err != nil {
		if private >= 0 {
			_ = unix.Close(private)
		}
		return -1, errors.New("cannot inspect local terminal")
	}
	return private, openErr
}

type declaredTerminalOps struct {
	stat       func(int, *unix.Stat_t) error
	open       func(string, int, uint32) (int, error)
	close      func(int) error
	isTerminal func(int) bool
}

func privateDeclaredTerminal(fd, direction int) (int, error) {
	return privateDeclaredTerminalWith(fd, direction, declaredTerminalOps{
		stat: unix.Fstat, open: unix.Open, close: unix.Close, isTerminal: term.IsTerminal,
	})
}

func privateDeclaredTerminalWith(fd, direction int, ops declaredTerminalOps) (int, error) {
	if direction != unix.O_RDONLY && direction != unix.O_WRONLY {
		return -1, errors.New("invalid local terminal direction")
	}
	var original unix.Stat_t
	if ops.stat(fd, &original) != nil || original.Mode&unix.S_IFMT != unix.S_IFCHR {
		return -1, errors.New("cannot identify local terminal")
	}
	private, err := ops.open("/proc/self/fd/"+strconv.Itoa(fd), direction|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, errors.New("cannot open private local terminal")
	}
	var reopened unix.Stat_t
	if ops.stat(private, &reopened) != nil || reopened.Mode&unix.S_IFMT != unix.S_IFCHR ||
		original.Dev != reopened.Dev || original.Ino != reopened.Ino || original.Rdev != reopened.Rdev || !ops.isTerminal(private) {
		_ = ops.close(private)
		return -1, errors.New("local terminal identity changed")
	}
	return private, nil
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

type declaredAttachment interface {
	Next(context.Context) (declaredsession.AttachEvent, error)
	ReadOnly() bool
	Input(context.Context, []byte) error
	Resize(context.Context, uint16, uint16) error
	Close() error
}

type declaredAttachInput struct {
	data []byte
	eof  bool
	err  error
}

func declaredAttachLoop(parent context.Context, attachment declaredAttachment, decoder *declaredDetachDecoder, inFD, outFD int) error {
	ctx, cancel := context.WithCancel(parent)
	results := make(chan declaredAttachResult, 1)
	inputs := make(chan declaredAttachInput, 1)
	done := make(chan struct{})
	inputDone := make(chan struct{})
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
	go func() {
		defer close(inputDone)
		readDeclaredTerminalInput(ctx, inFD, inputs)
	}()
	defer func() {
		cancel()
		_ = attachment.Close() // Closes only this viewer's connection.
		<-done
		<-inputDone // Both readers are joined before private descriptors close.
	}()
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, syscall.SIGWINCH)
	defer signal.Stop(resizes)
	return pumpDeclaredAttachment(ctx, attachment, decoder, results, inputs, resizes,
		func(data []byte) error { return writeDeclaredTerminal(ctx, outFD, data) },
		func() (uint16, uint16, error) { return declaredTerminalSize(inFD) })
}

// Terminal polling has its own bounded reader. Output arrival wakes the pump
// immediately instead of paying one 20ms stdin poll for every output frame.
func readDeclaredTerminalInput(ctx context.Context, fd int, inputs chan<- declaredAttachInput) {
	input := make([]byte, 8<<10)
	for {
		if ctx.Err() != nil {
			return
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 20)
		if err == unix.EINTR {
			continue
		}
		result := declaredAttachInput{}
		if err != nil || fds[0].Revents&unix.POLLNVAL != 0 {
			result.err = errors.New("local terminal input failed")
		} else if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
			continue
		} else {
			n, readErr := unix.Read(fd, input)
			if readErr == unix.EAGAIN || readErr == unix.EINTR {
				continue
			}
			if readErr != nil {
				result.err = errors.New("local terminal input failed")
			} else if n == 0 {
				result.eof = true
			} else {
				result.data = append([]byte(nil), input[:n]...)
			}
		}
		select {
		case inputs <- result:
		case <-ctx.Done():
			return
		}
		if result.eof || result.err != nil {
			return
		}
	}
}

const declaredAttachBurst = 64

func pumpDeclaredAttachment(ctx context.Context, attachment declaredAttachment, decoder *declaredDetachDecoder,
	results <-chan declaredAttachResult, inputs <-chan declaredAttachInput, resizes <-chan os.Signal,
	writeOutput func([]byte) error, terminalSize func() (uint16, uint16, error)) error {
	readOnlyNoticeShown := false
	noticeDiscard := func() error {
		if readOnlyNoticeShown {
			return nil
		}
		if err := writeOutput([]byte("\r\n[Citadel session is read-only; input is not forwarded.]\r\n")); err != nil {
			return err
		}
		readOnlyNoticeShown = true
		return nil
	}
	forward := func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if attachment.ReadOnly() {
			return noticeDiscard()
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
					return noticeDiscard()
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return errors.New("declared session holder input refused")
			}
			data = data[n:]
		}
		return nil
	}
	consumeInput := func(input declaredAttachInput) (bool, error) {
		if input.err != nil {
			return true, input.err
		}
		if input.eof {
			return true, forward(decoder.Flush())
		}
		data, detach := decoder.Feed(input.data)
		if err := forward(data); err != nil {
			return true, err
		}
		return detach, nil
	}
	resize := func() error {
		if attachment.ReadOnly() {
			return nil
		}
		cols, rows, err := terminalSize()
		if err != nil {
			return err
		}
		if err := attachment.Resize(ctx, cols, rows); err != nil && !errors.Is(err, declaredsession.ErrRole) {
			return errors.New("declared session holder resize refused")
		}
		return nil
	}
	serviceLocal := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		select {
		case input := <-inputs:
			if stop, err := consumeInput(input); stop {
				return true, err
			}
		default:
		}
		select {
		case <-resizes:
			if err := resize(); err != nil {
				return true, err
			}
		default:
		}
		return false, nil
	}
	handle := func(result declaredAttachResult) (bool, error) {
		if result.err != nil {
			if err := ctx.Err(); err != nil {
				return true, err
			}
			if errors.Is(result.err, io.EOF) {
				return true, errors.New("declared session holder disconnected")
			}
			if reason := declaredsession.AttachmentDisconnectReason(result.err); reason != "" {
				return true, errors.New("declared session holder disconnected: " + reason)
			}
			return true, errors.New("declared session holder operation failed")
		}
		switch result.event.Kind {
		case "stopped":
			switch result.event.Reason {
			case "", "operator_stop", "operator_restart", "child_exit":
				return true, nil
			case "slow_viewer", "protocol_error", "storage_failure", "cleanup_unconfirmed":
				return true, errors.New("declared session holder disconnected: " + result.event.Reason)
			default:
				return true, errors.New("declared session holder disconnected")
			}
		case "role":
			if !result.event.ReadOnly {
				readOnlyNoticeShown = false
				if err := resize(); err != nil {
					return true, err
				}
			}
		case "output":
			if err := writeOutput(result.event.Data); err != nil {
				return true, err
			}
		case "dimensions":
		default:
			return true, errors.New("declared session holder event refused")
		}
		// Consider ready detach/input/resize after every bounded output write,
		// even in a full burst. A slow terminal must not multiply the wait by
		// the entire burst budget before the local operator is heard.
		return serviceLocal()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resizes:
			if err := resize(); err != nil {
				return err
			}
		case result := <-results:
			if stop, err := handle(result); stop {
				return err
			}
			if stop, err := drainDeclaredAttachResults(ctx, results, declaredAttachBurst-1, handle); stop {
				return err
			}
		case input := <-inputs:
			if stop, err := consumeInput(input); stop {
				return err
			}
		}
	}
}

// Drain a ready burst without timers or a poll between frames, but yield after
// a fixed budget so sustained output cannot monopolize input and resize work.
func drainDeclaredAttachResults(ctx context.Context, results <-chan declaredAttachResult, budget int, handle func(declaredAttachResult) (bool, error)) (bool, error) {
	for i := 0; i < budget; i++ {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		select {
		case result := <-results:
			if stop, err := handle(result); stop {
				return true, err
			}
		default:
			return false, nil
		}
	}
	return false, nil
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
