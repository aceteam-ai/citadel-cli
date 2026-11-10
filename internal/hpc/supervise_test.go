//go:build !windows

package hpc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeProc is a scripted child. onSignal decides whether a signal ends it.
type fakeProc struct {
	name     string
	mu       sync.Mutex
	signals  []os.Signal
	done     chan error
	once     sync.Once
	onSignal func(os.Signal) (exit bool, err error)
	events   *eventLog
}

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) { l.mu.Lock(); l.ev = append(l.ev, s); l.mu.Unlock() }
func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func newFakeProc(name string, ev *eventLog) *fakeProc {
	return &fakeProc{name: name, done: make(chan error, 1), events: ev,
		onSignal: func(os.Signal) (bool, error) { return true, nil }}
}

func (p *fakeProc) exit(err error) {
	p.once.Do(func() { p.events.add(p.name + ":exit"); p.done <- err; close(p.done) })
}
func (p *fakeProc) Done() <-chan error { return p.done }
func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	p.events.add(p.name + ":" + sig.String())
	if exit, err := p.onSignal(sig); exit {
		go p.exit(err)
	}
	return nil
}

type harness struct {
	ev       *eventLog
	engine   *fakeProc
	worker   *fakeProc
	sigs     chan os.Signal
	ready    chan error
	started  atomic.Bool
	stopCall atomic.Int32
}

func newHarness() *harness {
	ev := &eventLog{}
	return &harness{ev: ev, engine: newFakeProc("engine", ev), worker: newFakeProc("worker", ev),
		sigs: make(chan os.Signal, 4), ready: make(chan error, 1)}
}

func (h *harness) supervisor() Supervisor {
	return Supervisor{
		PickPort:    func() (int, error) { return 40001, nil },
		StartEngine: func(int) (Process, error) { h.ev.add("engine:start"); return h.engine, nil },
		WaitReady: func(ctx context.Context, _ int, done <-chan error) error {
			select {
			case err := <-h.ready:
				return err
			case err := <-done:
				return errors.New("engine exited: " + errString(err))
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		StartWorker: func(port int) (Process, error) {
			h.started.Store(true)
			h.ev.add("worker:start:" + strconv.Itoa(port))
			return h.worker, nil
		},
		StopEngine: func(p Process) {
			h.stopCall.Add(1)
			h.ev.add("engine:stop")
			p.(*fakeProc).exit(nil)
		},
		Signals:      h.sigs,
		IsDrain:      func(s os.Signal) bool { return s == syscall.SIGUSR1 },
		ReadyTimeout: time.Second,
		DrainWait:    500 * time.Millisecond,
		TermWait:     200 * time.Millisecond,
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func runAsync(s Supervisor) chan [2]any {
	out := make(chan [2]any, 1)
	go func() {
		code, err := s.Run(context.Background())
		out <- [2]any{code, err}
	}()
	return out
}

func waitResult(t *testing.T, ch chan [2]any) (int, error) {
	t.Helper()
	select {
	case r := <-ch:
		err, _ := r[1].(error)
		return r[0].(int), err
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return")
		return 0, nil
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSupervisorDrainSequence(t *testing.T) {
	h := newHarness()
	res := runAsync(h.supervisor())
	h.ready <- nil
	waitFor(t, h.started.Load)
	h.sigs <- syscall.SIGUSR1
	code, err := waitResult(t, res)
	if code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	want := []string{"engine:start", "worker:start:40001", "worker:" + syscall.SIGUSR1.String(), "worker:exit", "engine:stop", "engine:exit"}
	if got := h.ev.list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v\nwant %v", got, want)
	}
}

func TestSupervisorWorkerIgnoringDrainIsKilled(t *testing.T) {
	h := newHarness()
	h.worker.onSignal = func(s os.Signal) (bool, error) { return s == syscall.SIGKILL, errors.New("killed") }
	res := runAsync(h.supervisor())
	h.ready <- nil
	waitFor(t, h.started.Load)
	h.sigs <- syscall.SIGUSR1
	if code, _ := waitResult(t, res); code != 0 {
		t.Fatalf("code %d", code)
	}
	if h.stopCall.Load() != 1 {
		t.Fatal("engine not stopped")
	}
	h.worker.mu.Lock()
	defer h.worker.mu.Unlock()
	if len(h.worker.signals) != 2 || h.worker.signals[1] != syscall.SIGKILL {
		t.Fatalf("worker signals %v", h.worker.signals)
	}
}

func TestSupervisorEngineDiesWhileServing(t *testing.T) {
	h := newHarness()
	res := runAsync(h.supervisor())
	h.ready <- nil
	waitFor(t, h.started.Load)
	h.engine.exit(errors.New("cuda oom"))
	code, err := waitResult(t, res)
	if code != 1 || err == nil {
		t.Fatalf("code %d err %v", code, err)
	}
	h.worker.mu.Lock()
	defer h.worker.mu.Unlock()
	if len(h.worker.signals) == 0 || h.worker.signals[0] != syscall.SIGTERM {
		t.Fatalf("worker was not asked to deregister: %v", h.worker.signals)
	}
}

func TestSupervisorEngineFailsBeforeReady(t *testing.T) {
	h := newHarness()
	res := runAsync(h.supervisor())
	h.engine.exit(errors.New("bad model"))
	code, err := waitResult(t, res)
	if code != 1 || err == nil || h.started.Load() {
		t.Fatalf("code %d err %v workerStarted %v", code, err, h.started.Load())
	}
}

func TestSupervisorReadyTimeout(t *testing.T) {
	h := newHarness()
	s := h.supervisor()
	s.ReadyTimeout = 50 * time.Millisecond
	code, err := waitResult(t, runAsync(s))
	if code != 1 || err == nil || h.started.Load() || h.stopCall.Load() != 1 {
		t.Fatalf("code %d err %v started %v stops %d", code, err, h.started.Load(), h.stopCall.Load())
	}
}

func TestSupervisorSignalBeforeReadyNeverEnrolls(t *testing.T) {
	h := newHarness()
	res := runAsync(h.supervisor())
	h.sigs <- syscall.SIGTERM
	code, err := waitResult(t, res)
	if code != 0 || err != nil || h.started.Load() || h.stopCall.Load() != 1 {
		t.Fatalf("code %d err %v started %v stops %d", code, err, h.started.Load(), h.stopCall.Load())
	}
}

func TestSupervisorWorkerCrash(t *testing.T) {
	h := newHarness()
	res := runAsync(h.supervisor())
	h.ready <- nil
	waitFor(t, h.started.Load)
	h.worker.exit(errors.New("exit status 1"))
	code, err := waitResult(t, res)
	if code != 1 || err == nil || h.stopCall.Load() != 1 {
		t.Fatalf("code %d err %v stops %d", code, err, h.stopCall.Load())
	}
}

func TestWaitModelsReady(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	if err := WaitModelsReady(context.Background(), srv.URL, time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	done <- errors.New("exited")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer dead.Close()
	if err := WaitModelsReady(context.Background(), dead.URL, time.Millisecond, done); err == nil {
		t.Fatal("an exited engine was reported ready")
	}
}

func TestEngineCommandBindsLoopback(t *testing.T) {
	argv, env := EngineSpec{VenvDir: "/v", Model: "m", HFHome: "/hf", DownloadDir: "/w", TensorParallel: 2, Args: []string{"--max-model-len=4096"}}.Command(40001)
	want := []string{"/v/bin/python", "-m", "vllm.entrypoints.openai.api_server", "--host", "127.0.0.1", "--port", "40001",
		"--model", "m", "--download-dir", "/w", "--tensor-parallel-size", "2", "--max-model-len=4096"}
	if !reflect.DeepEqual(argv, want) || env["HF_HOME"] != "/hf" {
		t.Fatalf("argv %v env %v", argv, env)
	}
}

func TestPickLoopbackPortAndNoProxy(t *testing.T) {
	port, err := PickLoopbackPort()
	if err != nil || port <= 0 {
		t.Fatalf("port %d err %v", port, err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("picked port is not free on loopback: %v", err)
	}
	l.Close()
	if got := LoopbackNoProxy("example.org, 127.0.0.1"); got != "example.org,127.0.0.1,localhost,::1" {
		t.Fatalf("NO_PROXY = %q", got)
	}
}
