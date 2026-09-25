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
