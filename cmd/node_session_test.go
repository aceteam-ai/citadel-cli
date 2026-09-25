package cmd

import (
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

func TestUnenrolledWorkDoesNotFixLaterDeviceAuthModeToPresence(t *testing.T) {
	dir := t.TempDir()
	// An exploratory work command on an unenrolled machine must fail without
	// writing the authkey-only default to session.yaml.
	if _, err := loadWorkSessionConfig(dir, false, false); err == nil {
		t.Fatal("unenrolled work unexpectedly started")
	}
	if _, err := os.Stat(nodesession.Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unenrolled work left a durable session mode: %v", err)
	}

	// The successful device-auth enroll path initializes the same file. The
	// next work invocation must pass the presence gate and enter the existing
	// worker startup path, where its job source and Runner are constructed.
	enrolled, err := nodesession.LoadOrInitialize(dir, true)
	if err != nil || enrolled.Mode != nodesession.Worker {
		t.Fatalf("device-auth enrollment mode = %+v, %v; want worker", enrolled, err)
	}
	startup, err := loadWorkSessionConfig(dir, true, true)
	if err != nil || startup.Mode != nodesession.Worker {
		t.Fatalf("subsequent work startup mode = %+v, %v; want worker", startup, err)
	}

	// A saved explicit choice still wins if credentials or enrollment tier
	// later change; the work command must never silently switch it.
	if err := nodesession.Save(dir, nodesession.Config{Mode: nodesession.Presence}); err != nil {
		t.Fatal(err)
	}
	startup, err = loadWorkSessionConfig(dir, true, true)
	if err != nil || startup.Mode != nodesession.Presence {
		t.Fatalf("explicit presence mode = %+v, %v; want presence", startup, err)
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
