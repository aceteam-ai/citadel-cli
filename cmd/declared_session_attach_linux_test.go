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
	inputErr error
	calls    int
}

func (*fakeDeclaredAttachment) Next(context.Context) (declaredsession.AttachEvent, error) {
	return declaredsession.AttachEvent{}, io.EOF
}

func (f *fakeDeclaredAttachment) ReadOnly() bool { return f.readonly }
func (f *fakeDeclaredAttachment) Input(_ context.Context, data []byte) error {
	f.calls++
	if f.inputErr != nil {
		return f.inputErr
	}
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

func isolateDeclaredAttachEnvironment(t *testing.T) {
	t.Helper()
	root := declaredCommandTempDir(t)
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, root)
	}
}

func TestDeclaredAttachReadOnlyDiscardNoticeIsStaticAndDoesNotReplayInput(t *testing.T) {
	isolateDeclaredAttachEnvironment(t)
	results := make(chan declaredAttachResult, 3)
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "role", ReadOnly: true, Reason: "untrusted-test-text"}}
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "role", ReadOnly: true}}
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "stopped"}}
	inputs := make(chan declaredAttachInput, 1)
	inputs <- declaredAttachInput{data: []byte("must-not-reach-child")}
	decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
	if err != nil {
		t.Fatal(err)
	}
	attachment := &fakeDeclaredAttachment{readonly: true}
	var output bytes.Buffer
	err = pumpDeclaredAttachment(context.Background(), attachment, decoder, results, inputs, nil,
		func(data []byte) error {
			_, err := output.Write(data)
			return err
		},
		func() (uint16, uint16, error) {
			t.Fatal("read-only role requested resize")
			return 0, 0, nil
		})
	const notice = "\r\n[Citadel session is read-only; input is not forwarded.]\r\n"
	if err != nil || output.String() != notice || len(attachment.input) != 0 || attachment.resizes != 0 {
		t.Fatalf("read-only discard was silent, repeated, reflected peer text or forwarded input: err=%v output=%q input=%q", err, output.String(), attachment.input)
	}
}

func TestDeclaredAttachGrantSequenceDoesNotReportDiscardedInput(t *testing.T) {
	isolateDeclaredAttachEnvironment(t)
	results := make(chan declaredAttachResult, 3)
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "role", ReadOnly: true}}
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "role", ReadOnly: false}}
	results <- declaredAttachResult{event: declaredsession.AttachEvent{Kind: "stopped"}}
	decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
	if err != nil {
		t.Fatal(err)
	}
	// A non-ceiling attachment receiving the ordinary attach/take-control
	// grant sequence has not discarded any input.
	attachment := &fakeDeclaredAttachment{}
	var output bytes.Buffer
	err = pumpDeclaredAttachment(context.Background(), attachment, decoder, results, nil, nil,
		func(data []byte) error { _, err := output.Write(data); return err },
		func() (uint16, uint16, error) { return 80, 24, nil })
	if err != nil || output.Len() != 0 || len(attachment.input) != 0 || attachment.resizes != 1 {
		t.Fatalf("normal grant falsely reported discarded input: err=%v output=%q resizes=%d", err, output.String(), attachment.resizes)
	}
}

func TestDeclaredAttachRoleRefusedInputShowsStaticNoticeWithoutReplay(t *testing.T) {
	for _, failOutput := range []bool{false, true} {
		isolateDeclaredAttachEnvironment(t)
		inputs := make(chan declaredAttachInput, 3)
		inputs <- declaredAttachInput{data: []byte("first")}
		inputs <- declaredAttachInput{data: []byte("second")}
		inputs <- declaredAttachInput{eof: true}
		decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
		if err != nil {
			t.Fatal(err)
		}
		attachment := &fakeDeclaredAttachment{inputErr: declaredsession.ErrRole}
		notices := 0
		outputErr := errors.New("fixture output unavailable")
		err = pumpDeclaredAttachment(context.Background(), attachment, decoder, nil, inputs, nil,
			func(data []byte) error {
				if string(data) != "\r\n[Citadel session is read-only; input is not forwarded.]\r\n" {
					t.Fatal("role-refused input reflected data instead of a static notice")
				}
				notices++
				if failOutput {
					return outputErr
				}
				return nil
			},
			func() (uint16, uint16, error) { t.Fatal("discard resized"); return 0, 0, nil })
		wantCalls := 2
		if failOutput {
			wantCalls = 1
		}
		if (err != nil) != failOutput || failOutput && !errors.Is(err, outputErr) ||
			notices != 1 || attachment.calls != wantCalls || len(attachment.input) != 0 {
			t.Fatalf("role refusal replayed input or lost notice failure: err=%v notices=%d calls=%d", err, notices, attachment.calls)
		}
	}
}

func TestDeclaredAttachDiscardNoticeResetsOnlyAfterWritableRole(t *testing.T) {
	isolateDeclaredAttachEnvironment(t)
	results := make(chan declaredAttachResult, 9)
	for _, event := range []declaredsession.AttachEvent{
		{Kind: "output", Data: []byte("drop-first")},
		{Kind: "output", Data: []byte("drop-again")},
		{Kind: "output", Data: []byte("grant")},
		{Kind: "role", ReadOnly: false},
		{Kind: "output", Data: []byte("deliver")},
		{Kind: "output", Data: []byte("revoke")},
		{Kind: "role", ReadOnly: true},
		{Kind: "output", Data: []byte("drop-later")},
		{Kind: "stopped"},
	} {
		results <- declaredAttachResult{event: event}
	}
	inputs := make(chan declaredAttachInput, 1)
	attachment := &fakeDeclaredAttachment{readonly: true}
	decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	err = pumpDeclaredAttachment(context.Background(), attachment, decoder, results, inputs, nil,
		func(data []byte) error {
			switch string(data) {
			case "drop-first", "drop-again", "drop-later":
				inputs <- declaredAttachInput{data: append([]byte(nil), data...)}
			case "grant":
				attachment.readonly = false
			case "deliver":
				inputs <- declaredAttachInput{data: []byte("delivered")}
			case "revoke":
				attachment.readonly = true
			case "\r\n[Citadel session is read-only; input is not forwarded.]\r\n":
				notices++
			default:
				t.Fatal("unexpected fixture output")
			}
			return nil
		},
		func() (uint16, uint16, error) { return 80, 24, nil })
	if err != nil || notices != 2 || string(attachment.input) != "delivered" || attachment.resizes != 1 {
		t.Fatalf("discard/restore/revoke sequence: err=%v notices=%d input=%q resizes=%d", err, notices, attachment.input, attachment.resizes)
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
		notice   bool
	}{
		{name: "detach", input: declaredAttachInput{data: []byte{'x', 0x1d, 'd', 'z'}}, want: []byte{'x'}},
		{name: "readonly detach", input: declaredAttachInput{data: []byte{'x', 0x1d, 'd', 'z'}}, readonly: true, notice: true},
		{name: "readonly chord only", input: declaredAttachInput{data: []byte{0x1d, 'd'}}, readonly: true},
		{name: "input EOF", input: declaredAttachInput{eof: true}, pending: true, want: []byte{0x1d}},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateDeclaredAttachEnvironment(t)
			inputs := make(chan declaredAttachInput, 1)
			inputs <- test.input
			decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
			if err != nil {
				t.Fatal(err)
			}
			decoder.pending = test.pending
			attachment := &fakeDeclaredAttachment{readonly: test.readonly}
			notices := 0
			err = pumpDeclaredAttachment(context.Background(), attachment, decoder, nil, inputs, nil,
				func(data []byte) error {
					if !test.notice || string(data) != "\r\n[Citadel session is read-only; input is not forwarded.]\r\n" {
						t.Fatal("local detach wrote unexpected output")
					}
					notices++
					return nil
				},
				func() (uint16, uint16, error) {
					t.Fatal("local detach resized")
					return 0, 0, nil
				})
			wantNotices := 0
			if test.notice {
				wantNotices = 1
			}
			if err != nil || !bytes.Equal(attachment.input, test.want) || notices != wantNotices {
				t.Fatalf("err=%v forwarded=%q want=%q notices=%d want=%d", err, attachment.input, test.want, notices, wantNotices)
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
