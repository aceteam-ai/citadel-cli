package clockstate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClockStateTimedatectlFixtures(t *testing.T) {
	for _, tc := range []struct {
		out   string
		state State
		valid bool
	}{
		{"NTPSynchronized=yes\n", Synced, true}, {"NTPSynchronized=no\n", Unsynced, true},
		{"", Unknown, false}, {"yes", Unknown, false}, {"NTPSynchronized=unknown", Unknown, false},
		{"NTPSynchronized=yes\nNTPSynchronized=no", Unknown, false}, {strings.Repeat("x", outputLimit+1), Unknown, false},
	} {
		t.Run(tc.out[:min(len(tc.out), 30)], func(t *testing.T) {
			state, valid := parseTimedatectl(tc.out)
			if state != tc.state || valid != tc.valid {
				t.Fatalf("got %q/%v", state, valid)
			}
		})
	}
}

func TestClockStateChronyFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, ref, leap string
		want            State
	}{
		{"external", "C0000201 (192.0.2.1)", "Normal", Synced},
		{"insert", "C0000201 (192.0.2.1)", "Insert second", Synced},
		{"delete", "C0000201 (192.0.2.1)", "Delete second", Synced},
		{"unsynced", "00000000 ()", "Not synchronised", Unsynced},
		{"local", "7F7F0101 ()", "Normal", Unknown},
		{"localNamed", "7F7F0101 (LOCAL)", "Normal", Unknown},
		{"missingExternal", "C0000201", "Normal", Unknown},
		{"invalidHex", "BAD_ID01 (192.0.2.1)", "Normal", Unknown},
		{"leapMalformed", "C0000201 (192.0.2.1)", "Normal maybe", Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseChronyc("Reference ID : " + tc.ref + "\nLeap status : " + tc.leap + "\n")
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	for _, out := range []string{"", "Leap status : Normal\nLeap status : Normal", "Reference ID : C0000201 (192.0.2.1)\nLeap status : Normal\nmalformed", strings.Repeat("x", outputLimit+1)} {
		if parseChronyc(out) != Unknown {
			t.Fatal("malformed chrony accepted")
		}
	}
}

func TestClockStateProbeFixedCommandsAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name, first string
		err         error
		want        State
		calls       int
	}{
		{"yes", "NTPSynchronized=yes", nil, Synced, 1},
		{"noDoesNotFallback", "NTPSynchronized=no", nil, Unsynced, 1},
		{"malformed", "garbage", nil, Synced, 2},
		{"missingBinary", "", errors.New("synthetic missing executable"), Synced, 2},
		{"nonzero", "", errors.New("synthetic exit"), Synced, 2},
		{"oversize", strings.Repeat("x", outputLimit+1), nil, Synced, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := probeWith(context.Background(), func(ctx context.Context, name string, args ...string) (string, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > commandBudget {
					t.Fatal("no per-command deadline")
				}
				if calls == 1 {
					if name != "timedatectl" || !reflect.DeepEqual(args, []string{"show", "-p", "NTPSynchronized"}) {
						t.Fatal("unexpected argv")
					}
					return tc.first, tc.err
				}
				if name != "chronyc" || !reflect.DeepEqual(args, []string{"tracking"}) {
					t.Fatal("unexpected fallback argv")
				}
				return "Reference ID : C0000201 (192.0.2.1)\nLeap status : Normal", nil
			})
			if got != tc.want || calls != tc.calls {
				t.Fatalf("got %q calls%d", got, calls)
			}
		})
	}
}

func TestClockStateProbeFakeCancellationAndMissingBoth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if got := probeWith(ctx, func(ctx context.Context, _ string, _ ...string) (string, error) {
		calls++
		<-ctx.Done()
		return "NTPSynchronized=yes", nil
	}); got != Unknown || calls != 1 {
		t.Fatal("cancelled report accepted or fallback attempted")
	}
	if got := probeWith(context.Background(), func(context.Context, string, ...string) (string, error) {
		return "", errors.New("synthetic missing binary")
	}); got != Unknown {
		t.Fatal("missing commands must be unknown")
	}
}

func TestClockStateCommandConstructionAndCapAreInert(t *testing.T) {
	cmd := clockCommand(context.Background(), "/fixture-only/missing-clock", "show", "-p", "NTPSynchronized")
	if cmd.WaitDelay != 100*time.Millisecond || !reflect.DeepEqual(cmd.Env, []string{"LC_ALL=C"}) || cmd.Cancel == nil {
		t.Fatal("command environment/grace changed")
	}
	if !reflect.DeepEqual(cmd.Args, []string{"/fixture-only/missing-clock", "show", "-p", "NTPSynchronized"}) {
		t.Fatal("argv changed")
	}
	var b cappedOutput
	if n, err := b.Write(make([]byte, outputLimit)); n != outputLimit || err != nil {
		t.Fatal("exact cap rejected")
	}
	if _, err := b.Write([]byte("x")); err == nil || b.Len() != outputLimit {
		t.Fatal("oversize accepted")
	}
}
