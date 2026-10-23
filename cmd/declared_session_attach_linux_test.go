//go:build linux

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"golang.org/x/sys/unix"
)

// These exercise the same event pump as the terminal client, without opening
// a real terminal, socket, node configuration, holder or process.
type fakeDeclaredAttachment struct {
	readonly bool
	input    []byte
	resizes  int
}

func (*fakeDeclaredAttachment) Next(context.Context) (declaredsession.AttachEvent, error) {
	return declaredsession.AttachEvent{}, io.EOF
}

func (f *fakeDeclaredAttachment) ReadOnly() bool { return f.readonly }
func (f *fakeDeclaredAttachment) Input(_ context.Context, data []byte) error {
	f.input = append(f.input, data...)
	return nil
}
func (f *fakeDeclaredAttachment) Resize(context.Context, uint16, uint16) error {
	f.resizes++
	return nil
}
func (*fakeDeclaredAttachment) Close() error { return nil }

func TestDeclaredAttachPumpDrainsBurstWithoutInputPoll(t *testing.T) {
	const count = 4 * declaredAttachBurst
	results := make(chan declaredAttachResult, count+1)
	for i := 0; i < count; i++ {
		results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "output", Data: []byte{byte(i)}}}
	}
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "stopped"}}
	decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = pumpDeclaredAttachment(context.Background(), &fakeDeclaredAttachment{}, decoder,
		results, nil, nil, func(data []byte) error {
			_, err := output.Write(data)
			return err
		},
		func() (uint16, uint16, error) {
			t.Fatal("burst requested terminal dimensions")
			return 0, 0, nil
		})
	if err != nil || output.Len() != count {
		t.Fatalf("err=%v output=%d want=%d", err, output.Len(), count)
	}
	for i, value := range output.Bytes() {
		if value != byte(i) {
			t.Fatal("burst reordered or dropped output")
		}
	}
}

func TestDeclaredAttachBurstBudgetAndCancellation(t *testing.T) {
	results := make(chan declaredAttachResult, 2*declaredAttachBurst)
	for i := 0; i < cap(results); i++ {
		results <- declaredAttachResult{}
	}
	count := 0
	stop, err := drainDeclaredAttachResults(context.Background(), results, declaredAttachBurst, func(declaredAttachResult) (bool, error) {
		count++
		return false, nil
	})
	if stop || err != nil || count != declaredAttachBurst || len(results) != declaredAttachBurst {
		t.Fatalf("budget not honored: stop=%v err=%v count=%d remaining=%d", stop, err, count, len(results))
	}
	ctx, cancel := context.WithCancel(context.Background())
	count = 0
	stop, err = drainDeclaredAttachResults(ctx, results, declaredAttachBurst, func(declaredAttachResult) (bool, error) {
		count++
		cancel()
		return false, nil
	})
	if !stop || !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancellation starved: stop=%v err=%v count=%d", stop, err, count)
	}
}

func TestDeclaredAttachReadyDetachInterruptsBurstAfterCurrentOutput(t *testing.T) {
	results := make(chan declaredAttachResult, declaredAttachBurst)
	for i := 0; i < cap(results); i++ {
		results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "output", Data: []byte{'x'}}}
	}
	inputs := make(chan declaredAttachInput, 1)
	decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	err = pumpDeclaredAttachment(context.Background(), &fakeDeclaredAttachment{}, decoder, results, inputs, nil,
		func([]byte) error {
			writes++
			if writes == 1 {
				inputs <- declaredAttachInput{data: []byte{0x1d, 'd'}}
			}
			return nil
		},
		func() (uint16, uint16, error) {
			t.Fatal("detach resized")
			return 0, 0, nil
		})
	if err != nil || writes != 1 || len(results) != declaredAttachBurst-1 {
		t.Fatalf("ready detach waited for burst: err=%v writes=%d remaining=%d", err, writes, len(results))
	}
}

func TestDeclaredAttachPumpDisconnectsAreFailuresAndDoNotExposePeerText(t *testing.T) {
	for _, test := range []struct {
		name   string
		result declaredAttachResult
		want   string
	}{
		{name: "EOF", result: declaredAttachResult{err: io.EOF}, want: "holder disconnected"},
		{name: "protocol rejection", result: declaredAttachResult{err: declaredsession.ErrProtocol}, want: "holder operation failed"},
		{name: "untrusted error", result: declaredAttachResult{err: errors.New("private-test-secret")}, want: "holder operation failed"},
		{name: "slow viewer", result: declaredAttachResult{event: declaredsession.AttachEvent{Kind: "stopped", Reason: "slow_viewer"}}, want: "slow_viewer"},
		{name: "untrusted reason", result: declaredAttachResult{event: declaredsession.AttachEvent{Kind: "stopped", Reason: "private-test-secret"}}, want: "holder disconnected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := make(chan declaredAttachResult, 1)
			results <- test.result
			decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
			if err != nil {
				t.Fatal(err)
			}
			err = pumpDeclaredAttachment(context.Background(), &fakeDeclaredAttachment{}, decoder,
				results, nil, nil, func([]byte) error {
					t.Fatal("disconnect wrote output")
					return nil
				},
				func() (uint16, uint16, error) {
					t.Fatal("disconnect resized")
					return 0, 0, nil
				})
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "private-test-secret") {
				t.Fatalf("disconnect did not safely fail: %v", err)
			}
		})
	}
}

func TestDeclaredAttachPumpExplicitDetachAndInputEOFRemainSuccessful(t *testing.T) {
	for _, test := range []struct {
		name     string
		input    declaredAttachInput
		readonly bool
		pending  bool
		want     []byte
	}{
		{name: "detach", input: declaredAttachInput{data: []byte{'x', 0x1d, 'd', 'z'}}, want: []byte{'x'}},
		{name: "readonly detach", input: declaredAttachInput{data: []byte{'x', 0x1d, 'd', 'z'}}, readonly: true},
		{name: "input EOF", input: declaredAttachInput{eof: true}, pending: true, want: []byte{0x1d}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputs := make(chan declaredAttachInput, 1)
			inputs <- test.input
			decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
			if err != nil {
				t.Fatal(err)
			}
			decoder.pending = test.pending
			attachment := &fakeDeclaredAttachment{readonly: test.readonly}
			err = pumpDeclaredAttachment(context.Background(), attachment, decoder, nil, inputs, nil,
				func([]byte) error {
					t.Fatal("local detach wrote output")
					return nil
				},
				func() (uint16, uint16, error) {
					t.Fatal("local detach resized")
					return 0, 0, nil
				})
			if err != nil || !bytes.Equal(attachment.input, test.want) {
				t.Fatalf("err=%v forwarded=%q want=%q", err, attachment.input, test.want)
			}
		})
	}
}

func TestPrivateDeclaredTerminalUsesIndependentValidatedDescription(t *testing.T) {
	for _, direction := range []int{unix.O_RDONLY, unix.O_WRONLY} {
		original := unix.Stat_t{Mode: unix.S_IFCHR | 0o600, Dev: 1, Ino: 2, Rdev: 3}
		opened, closed := 0, 0
		ops := declaredTerminalOps{
			stat: func(fd int, stat *unix.Stat_t) error {
				if fd != 7 && fd != 101 {
					t.Fatal("unexpected descriptor inspected")
				}
				*stat = original
				return nil
			},
			open: func(path string, flags int, mode uint32) (int, error) {
				opened++
				if path != "/proc/self/fd/7" || flags != direction|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK || mode != 0 {
					t.Fatal("private terminal used a different path or flags")
				}
				return 101, nil
			},
			close: func(int) error {
				closed++
				return nil
			},
			isTerminal: func(fd int) bool { return fd == 101 },
		}
		fd, err := privateDeclaredTerminalWith(7, direction, ops)
		if err != nil || fd != 101 || opened != 1 || closed != 0 {
			t.Fatalf("fd=%d err=%v opened=%d closed=%d", fd, err, opened, closed)
		}
	}
}

func TestPrivateDeclaredTerminalRefusesEveryIdentityMismatch(t *testing.T) {
	for _, mutate := range []func(*unix.Stat_t){
		func(stat *unix.Stat_t) { stat.Dev++ },
		func(stat *unix.Stat_t) { stat.Ino++ },
		func(stat *unix.Stat_t) { stat.Rdev++ },
		func(stat *unix.Stat_t) { stat.Mode = unix.S_IFREG | 0o600 },
	} {
		closed := 0
		fd, err := privateDeclaredTerminalWith(7, unix.O_RDONLY, declaredTerminalOps{
			stat: func(fd int, stat *unix.Stat_t) error {
				*stat = unix.Stat_t{Mode: unix.S_IFCHR | 0o600, Dev: 1, Ino: 2, Rdev: 3}
				if fd == 101 {
					mutate(stat)
				}
				return nil
			},
			open: func(string, int, uint32) (int, error) {
				return 101, nil
			},
			close: func(fd int) error {
				if fd != 101 {
					t.Fatal("attempted to close the inherited descriptor")
				}
				closed++
				return nil
			},
			isTerminal: func(int) bool { return true },
		})
		if err == nil || fd != -1 || closed != 1 {
			t.Fatalf("identity mismatch accepted: fd=%d err=%v closed=%d", fd, err, closed)
		}
	}
}

func TestPrivateDeclaredTerminalRefusesNonTTYAndUnsupportedDirection(t *testing.T) {
	closed := 0
	ops := declaredTerminalOps{
		stat: func(_ int, stat *unix.Stat_t) error {
			*stat = unix.Stat_t{Mode: unix.S_IFCHR | 0o600, Dev: 1, Ino: 2, Rdev: 3}
			return nil
		},
		open: func(string, int, uint32) (int, error) {
			return 101, nil
		},
		close: func(int) error {
			closed++
			return nil
		},
		isTerminal: func(int) bool { return false },
	}
	fd, err := privateDeclaredTerminalWith(7, unix.O_RDONLY, ops)
	if err == nil || fd != -1 || closed != 1 {
		t.Fatalf("non-TTY accepted: fd=%d err=%v closed=%d", fd, err, closed)
	}
	fd, err = privateDeclaredTerminalWith(7, unix.O_RDWR, declaredTerminalOps{})
	if err == nil || fd != -1 {
		t.Fatalf("unsupported access direction accepted: fd=%d err=%v", fd, err)
	}
}

var _ declaredAttachment = (*fakeDeclaredAttachment)(nil)
