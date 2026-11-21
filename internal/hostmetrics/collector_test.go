package hostmetrics

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

type sourceFunc func(context.Context, *Budget) (Snapshot, error)

func (f sourceFunc) Read(c context.Context, b *Budget) (Snapshot, error) { return f(c, b) }
func testLimits() Limits {
	return Limits{Timeout: time.Second, MaxReadBytes: 1 << 20, MaxFileBytes: 64 << 10, MaxFiles: 96, MaxSlots: 8, MaxSeries: 256, MaxOutputBytes: 64 << 10}
}
func collectSnapshot(t *testing.T, s Snapshot) (string, error) {
	t.Helper()
	c, e := New("1297", testLimits(), sourceFunc(func(context.Context, *Budget) (Snapshot, error) { return s, nil }))
	if e != nil {
		t.Fatal(e)
	}
	return c.Collect(context.Background())
}
func TestUnknownSensorsHaveNoFabricatedZero(t *testing.T) {
	out, e := collectSnapshot(t, Snapshot{})
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range families {
		if !strings.Contains(out, `family="`+f+`"} 0`) {
			t.Fatalf("missing unknown family %s", f)
		}
	}
	if strings.Contains(out, "citadel_gpu_memory_used_bytes") || strings.Contains(out, "citadel_temperature_celsius") {
		t.Fatal("missing sensor became a zero")
	}
}
func TestLabelsAreFiniteAndDoNotExposeSourceIdentity(t *testing.T) {
	s := Snapshot{Groups: map[Family]Group{Disk: {true, []Reading{{DiskReadBytes, "private-user-secret-token-device", 100}}}, GPU: {true, []Reading{{GPUUtilizationRatio, "GPU-secret-uuid", 0.5}}}}}
	out, e := collectSnapshot(t, s)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out, "private") || strings.Contains(out, "secret") || strings.Contains(out, "GPU-") {
		t.Fatal("private source identity leaked")
	}
	if !strings.Contains(out, `citadel_disk_read_bytes_total{node_id="1297",slot="0"} 100`) {
		t.Fatal(out)
	}
}
func TestInvalidSamplesRefuseWholeScrape(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Snapshot
	}{
		{"unknown family", Snapshot{Groups: map[Family]Group{familyCount: {}}}},
		{"unknown contains value", Snapshot{Groups: map[Family]Group{CPU: {false, []Reading{{CPUTicks, "host", 1}}}}}},
		{"wrong family", Snapshot{Groups: map[Family]Group{Memory: {true, []Reading{{CPUTicks, "host", 1}}}}}},
		{"invalid metric", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{255, "host", 1}}}}}},
		{"NaN", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{CPUTicks, "host", math.NaN()}}}}}},
		{"Inf", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{CPUTicks, "host", math.Inf(1)}}}}}},
		{"negative counter", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{CPUTicks, "host", -1}}}}}},
		{"invalid ratio", Snapshot{Groups: map[Family]Group{GPU: {true, []Reading{{GPUUtilizationRatio, "gpu", 2}}}}}},
		{"empty identity", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{CPUTicks, "", 1}}}}}},
		{"huge identity", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{CPUTicks, strings.Repeat("x", 129), 1}}}}}},
		{"duplicate", Snapshot{Groups: map[Family]Group{CPU: {true, []Reading{{CPUTicks, "host", 1}, {CPUTicks, "host", 2}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, e := collectSnapshot(t, tc.s)
			if e == nil || out != "" {
				t.Fatalf("published invalid samples: %q %v", out, e)
			}
		})
	}
}
func TestLimitsAndIdentityAreMandatory(t *testing.T) {
	src := sourceFunc(func(context.Context, *Budget) (Snapshot, error) { return Snapshot{}, nil })
	for _, id := range []string{"", "node-name", "0", "1\nsecret", "-1", strings.Repeat("1", 21)} {
		if _, e := New(id, testLimits(), src); e == nil {
			t.Fatal("accepted invalid identity")
		}
	}
	for _, mutate := range []func(*Limits){func(l *Limits) { l.Timeout = 0 }, func(l *Limits) { l.Timeout = 6 * time.Second }, func(l *Limits) { l.MaxSlots = 65 }, func(l *Limits) { l.MaxSeries = 4097 }, func(l *Limits) { l.MaxReadBytes = 0 }, func(l *Limits) { l.MaxFileBytes = 0 }, func(l *Limits) { l.MaxFiles = 129 }, func(l *Limits) { l.MaxOutputBytes = 513 << 10 }} {
		l := testLimits()
		mutate(&l)
		if _, e := New("1", l, src); e == nil {
			t.Fatal("accepted invalid limits")
		}
	}
}
func TestCapacityRefusalsAndStableSlots(t *testing.T) {
	current := Snapshot{Groups: map[Family]Group{Disk: {true, []Reading{{DiskReadBytes, "a", 1}}}}}
	limits := testLimits()
	limits.MaxSlots = 1
	c, _ := New("1", limits, sourceFunc(func(context.Context, *Budget) (Snapshot, error) { return current, nil }))
	if _, e := c.Collect(context.Background()); e != nil {
		t.Fatal(e)
	}
	current.Groups[Disk] = Group{true, []Reading{{DiskReadBytes, "b", 2}}}
	if out, e := c.Collect(context.Background()); e == nil || out != "" {
		t.Fatal("unbounded device churn")
	}
	current.Groups[Disk] = Group{true, []Reading{{DiskReadBytes, "a", 3}}}
	if out, e := c.Collect(context.Background()); e != nil || !strings.Contains(out, `slot="0"} 3`) {
		t.Fatal(out, e)
	}
	limits = testLimits()
	limits.MaxSeries = 9
	c, _ = New("1", limits, sourceFunc(func(context.Context, *Budget) (Snapshot, error) { return current, nil }))
	if out, e := c.Collect(context.Background()); e == nil || out != "" {
		t.Fatal("series limit not enforced")
	}
	limits = testLimits()
	limits.MaxOutputBytes = 256
	c, _ = New("1", limits, sourceFunc(func(context.Context, *Budget) (Snapshot, error) { return Snapshot{}, nil }))
	if out, e := c.Collect(context.Background()); e == nil || out != "" {
		t.Fatal("output limit not enforced")
	}
}
func TestCancelledOrBusyCollectionNeverStartsAnotherRead(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	src := sourceFunc(func(ctx context.Context, _ *Budget) (Snapshot, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Snapshot{}, nil
	})
	c, _ := New("1", testLimits(), src)
	done := make(chan error, 1)
	go func() { _, e := c.Collect(context.Background()); done <- e }()
	<-entered
	if _, e := c.Collect(context.Background()); e == nil {
		t.Fatal("overlapping collection")
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Collect(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestSourceDeadlineAndErrorDoNotPublish(t *testing.T) {
	limits := testLimits()
	limits.Timeout = time.Millisecond
	c, _ := New("1", limits, sourceFunc(func(ctx context.Context, _ *Budget) (Snapshot, error) { <-ctx.Done(); return Snapshot{}, nil }))
	if out, e := c.Collect(context.Background()); out != "" || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(out, e)
	}
	c, _ = New("1", testLimits(), sourceFunc(func(context.Context, *Budget) (Snapshot, error) {
		return Snapshot{}, errors.New("secret underlying cause")
	}))
	if out, e := c.Collect(context.Background()); out != "" || e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal(out, e)
	}
}
