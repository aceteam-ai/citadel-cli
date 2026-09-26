package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
)

func TestPresenceLoopHoldsMeshWithoutJobSubscriptionUntilStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan struct{})
	served := make(chan struct{})
	cleaned := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- presenceLoop(ctx,
			func() bool { return true },
			func(context.Context) (bool, error) { close(connected); return true, nil },
			func() (func(), error) { close(served); return func() { close(cleaned) }, nil },
		)
	}()
	<-connected
	<-served
	select {
	case err := <-done:
		t.Fatalf("presence exited while mesh was live: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("presence did not stop")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("presence did not close its control endpoint")
	}
	if err := presenceLoop(context.Background(), func() bool { return false }, func(context.Context) (bool, error) {
		t.Fatal("unenrolled presence attempted mesh connect")
		return false, nil
	}, func() (func(), error) { t.Fatal("unenrolled presence published control"); return nil, nil }); err == nil {
		t.Fatal("unenrolled presence was accepted")
	}
	if err := presenceLoop(context.Background(), func() bool { return true }, func(context.Context) (bool, error) {
		return false, errors.New("offline")
	}, func() (func(), error) { t.Fatal("offline presence published control"); return nil, nil }); err == nil {
		t.Fatal("offline presence was accepted")
	}
}

func TestWorkForcesWorkerRegardlessOfPersistedPresenceMode(t *testing.T) {
	// citadel work (and the citadel-worker systemd unit) serves jobs regardless
	// of the persisted session mode: its mode is the workSessionMode constant,
	// never derived from session.yaml. This assertion documents the bare-path /
	// work-path divergence on identical persisted state; it is not by itself a
	// proof about runWork (a compile-time const), which the source pins below
	// cover.
	if workSessionMode != nodesession.Worker {
		t.Fatalf("citadel work session mode = %q; want worker", workSessionMode)
	}

	// The reviewer's regression sequence: a stock authkey `citadel work` (whose
	// enrollment persisted Presence) used to read session.yaml, re-persist the
	// Presence default, and dispatch to runPresence, so a node whose systemd unit
	// is literally named citadel-worker came up presence-only and served zero
	// jobs. runWork must now do NEITHER: it must not initialize session.yaml
	// (enrollment owns that file) and must not branch to presence. runWork is not
	// unit-testable, so pin it at the source level -- this fails on exactly the
	// edit that reintroduces the branch.
	work, err := os.ReadFile("work.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(work, []byte("runPresence(")) {
		t.Fatal("cmd/work.go dispatches to runPresence; citadel work must always run as a worker")
	}
	if bytes.Contains(work, []byte("LoadOrInitialize(")) {
		t.Fatal("cmd/work.go initializes session.yaml; enrollment (citadel init) owns that file, not citadel work")
	}

	// The persisted Presence mode is not orphaned: the bare-`citadel` dispatch
	// path still reads it and still routes to presence. That divergence on
	// identical persisted state is the whole point.
	bare, err := os.ReadFile("bare_dispatch.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(bare, []byte("LoadOrInitialize(")) || !bytes.Contains(bare, []byte("runPresence(")) {
		t.Fatal("cmd/bare_dispatch.go must still read the persisted mode and route to presence")
	}
}

func TestPresenceLoopSurvivesSessionControlFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- presenceLoop(ctx,
			func() bool { return true },
			func(context.Context) (bool, error) { return true, nil },
			// A failed control listener (realistically EACCES on a root-owned
			// state dir) is best-effort: presence must keep holding the mesh
			// connection, never abort.
			func() (func(), error) { return nil, errors.New("bind: permission denied") },
		)
	}()
	select {
	case err := <-done:
		t.Fatalf("presence exited on a session-control bind failure: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("presence returned error after clean stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("presence did not stop after cancel")
	}
}

func TestNodeSessionStatusAndExplicitStopOnSharedLocalAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixed global named pipe cannot be isolated in parallel tests")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := serveNodeSessionControl(dir, nodesession.Presence, cancel, func(context.Context) nodeSessionStatus {
		return nodeSessionStatus{Connected: true, NodeName: "test-node", MeshIP: "100.64.0.12"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer requestCancel()
	resp, err := network.SessionControlRequest(requestCtx, dir, http.MethodGet, sessionStatusRoute)
	if err != nil {
		t.Fatal(err)
	}
	var got nodeSessionStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Mode != nodesession.Presence || !got.Connected || got.NodeName != "test-node" || got.MeshIP != "100.64.0.12" {
		t.Fatalf("session status = %+v", got)
	}
	if ctx.Err() != nil {
		t.Fatal("status unexpectedly stopped the session")
	}
	info, err := os.Stat(network.LocalAPISocketPath(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("control socket permissions: %v, %v", info, err)
	}
	resp, err = network.SessionControlRequest(requestCtx, dir, http.MethodGet, sessionStopRoute)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET stop = %v, %v", resp, err)
	}
	resp.Body.Close()
	if ctx.Err() != nil {
		t.Fatal("non-POST request stopped the session")
	}
	resp, err = network.SessionControlRequest(requestCtx, dir, http.MethodPost, sessionStopRoute)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST stop = %v, %v", resp, err)
	}
	resp.Body.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("acknowledged stop did not cancel the session")
	}
}
