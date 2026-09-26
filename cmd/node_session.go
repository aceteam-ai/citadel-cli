package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/spf13/cobra"
)

const (
	sessionStatusRoute = "/citadel/session/v1/status"
	sessionStopRoute   = "/citadel/session/v1/stop"
)

type nodeSessionStatus struct {
	Mode      nodesession.Mode `json:"mode"`
	Connected bool             `json:"connected"`
	NodeName  string           `json:"node_name,omitempty"`
	MeshIP    string           `json:"mesh_ip,omitempty"`
	Version   string           `json:"version"`
	PID       int              `json:"pid"`
	UptimeSec int64            `json:"uptime_seconds"`
}

// startNodeSessionControl serves only two S2 verbs over the exact existing
// localapi path. The OS-enforced 0600 socket / admin-only Windows named pipe is
// the authority boundary; no unauthenticated loopback or mesh HTTP listener is
// added. S3 will add client attach; S5 will add mode changes.
func startNodeSessionControl(mode nodesession.Mode, cancel context.CancelFunc) (func(), error) {
	mesh := network.Global()
	if mesh == nil || mesh.Mode() != network.ModeUserspace {
		return nil, errors.New("node session control requires an owned userspace mesh; machine-wide TUN is separate")
	}
	return serveNodeSessionControl(network.GetStateDir(), mode, cancel, func(ctx context.Context) nodeSessionStatus {
		status := nodeSessionStatus{}
		if meshStatus, err := network.GetGlobalStatus(ctx); err == nil && meshStatus != nil {
			status.Connected = meshStatus.Connected
			status.NodeName = meshStatus.Hostname
		}
		if ip, err := network.GetGlobalIPv4(); err == nil {
			status.MeshIP = ip
		}
		return status
	})
}

func serveNodeSessionControl(stateDir string, mode nodesession.Mode, cancel context.CancelFunc, liveStatus func(context.Context) nodeSessionStatus) (func(), error) {
	ln, err := network.ListenSessionControl(stateDir)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+sessionStatusRoute, func(w http.ResponseWriter, r *http.Request) {
		status := liveStatus(r.Context())
		status.Mode, status.Version, status.PID, status.UptimeSec = mode, Version, os.Getpid(), int64(time.Since(started).Seconds())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("POST "+sessionStopRoute, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("{\"stopping\":true}\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // acknowledge before tearing down the transport
		}
		go cancel()
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	stop := func() {
		_ = srv.Close()
		network.CloseSessionControl(ln, stateDir)
		<-done
	}
	// The caller closes the controller before dropping its worklock and mesh
	// holder, so a TUN process cannot seize the endpoint during cleanup.
	return stop, nil
}

// runPresence is structurally unable to subscribe a queue: it only takes the
// existing single-instance worklock, restores the saved mesh identity, hosts
// local session control, and waits. No job source or worker.Runner is built.
func runPresence() error {
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lock, err := worklock.Acquire(network.GetStateDir(), Version, Log)
	if err != nil {
		return fmt.Errorf("cannot own presence session: %w", err)
	}
	defer lock.Release()
	defer network.Disconnect()
	return presenceLoop(ctx, network.HasState, network.VerifyOrReconnect, func() (func(), error) {
		return startNodeSessionControl(nodesession.Presence, cancel)
	})
}

// presenceLoop's dependencies deliberately contain no job source or Runner.
// Holding a mesh connection is its only ongoing operation.
func presenceLoop(ctx context.Context, hasState func() bool, verify func(context.Context) (bool, error), serveControl func() (func(), error)) error {
	if !hasState() {
		return errors.New("node is not enrolled; run 'citadel enroll' or 'citadel login --authkey' first")
	}
	connected, err := verify(ctx)
	if err != nil {
		return fmt.Errorf("cannot restore node mesh presence: %w; try 'citadel reconnect'", err)
	}
	if !connected {
		return errors.New("node mesh did not connect; try 'citadel reconnect'")
	}
	// Best-effort, matching the egress-relay auto-start precedent ("a failed
	// optional listener must never fail the worker"). The local session-control
	// socket is an observability/control convenience; presence's real job is to
	// hold the mesh connection. A bind failure (realistically EACCES on a
	// root-owned state dir) must not abort the presence session.
	if stopControl, err := serveControl(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: node session control unavailable (continuing without it): %v\n", err)
	} else {
		defer stopControl()
	}
	fmt.Println("Citadel presence online (no job subscription). Use 'citadel session stop' to stop it.")
	<-ctx.Done()
	return nil
}

var sessionCmd = &cobra.Command{Use: "session", Short: "Inspect or stop the local node session", Args: cobra.NoArgs}

var sessionStatusCmd = &cobra.Command{
	Use: "status", Short: "Read the running node session over the local control socket", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
		defer cancel()
		resp, err := network.SessionControlRequest(ctx, network.GetStateDir(), http.MethodGet, sessionStatusRoute)
		if err != nil {
			return fmt.Errorf("node session is unavailable: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("node session status: HTTP %d", resp.StatusCode)
		}
		var status nodeSessionStatus
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
	},
}

var sessionStopCmd = &cobra.Command{
	Use: "stop", Short: "Stop the running local node session", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
		defer cancel()
		resp, err := network.SessionControlRequest(ctx, network.GetStateDir(), http.MethodPost, sessionStopRoute)
		if err != nil {
			return fmt.Errorf("node session is unavailable: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("node session stop: HTTP %d", resp.StatusCode)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Node session stopping")
		return nil
	},
}

func init() {
	sessionCmd.AddCommand(sessionStatusCmd, sessionStopCmd)
	rootCmd.AddCommand(sessionCmd)
}
