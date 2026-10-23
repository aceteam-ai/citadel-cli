package cmd

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

const controlCenterWorkerDetailInterval = 5 * time.Second

// These observations belong to the monitor, never to this process's consume
// loop. ccWorkerMu protects all three values and the existing worker state.
var (
	ccSharedWorkerMonitor    bool
	ccSharedWorkerDetail     *status.WorkerDetail
	ccSharedWorkerObservedAt time.Time
)

func resetControlCenterWorkerDetail() {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	ccSharedWorkerMonitor = false
	ccSharedWorkerDetail = nil
	ccSharedWorkerObservedAt = time.Time{}
}

func controlCenterWorkerDetailSnapshot() (bool, *status.WorkerDetail, time.Time) {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	if ccSharedWorkerDetail == nil {
		return ccSharedWorkerMonitor, nil, time.Time{}
	}
	copy, err := status.CloneWorkerDetail(*ccSharedWorkerDetail)
	if err != nil {
		return ccSharedWorkerMonitor, nil, time.Time{}
	}
	return ccSharedWorkerMonitor, &copy, ccSharedWorkerObservedAt
}

// loadSessionWorkerDetail never exposes peer-controlled error bodies. A legacy
// owner without this opt-in route is unavailable, rather than a healthy zero.
func loadSessionWorkerDetail(ctx context.Context, stateDir string, request sessionStatusRequester) (*status.WorkerDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	response, err := request(ctx, stateDir)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK {
		return nil, status.ErrWorkerDetailUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, status.WorkerDetailMaxBytes+1))
	if err != nil || len(body) > status.WorkerDetailMaxBytes || ctx.Err() != nil {
		return nil, status.ErrWorkerDetailUnavailable
	}
	detail, err := status.DecodeWorkerDetail(body)
	if err != nil || ctx.Err() != nil {
		return nil, status.ErrWorkerDetailUnavailable
	}
	return &detail, nil
}

// This pins observations from the existing status identity, not an authenticated
// incarnation. PID reuse with identical observable fields remains indistinguishable.
type sessionWorkerDetailObserver struct {
	pid       int
	owner     *status.WorkerDetail
	changed   bool
	lastAlive int64
}

func (observer *sessionWorkerDetailObserver) accept(detail *status.WorkerDetail) bool {
	if observer.changed || detail == nil {
		return false
	}
	if detail.PID != observer.pid || detail.Mode != "worker" {
		observer.changed = true
		return false
	}
	if observer.owner != nil && (detail.Mode != observer.owner.Mode || detail.Version != observer.owner.Version ||
		detail.NodeName != observer.owner.NodeName || detail.UptimeSec < observer.lastAlive) {
		observer.changed = true
		return false
	}
	// Mesh IP and connectivity may change during reconnect without replacing
	// this observed worker. They are displayed data, not owner identity proofs.
	if observer.owner == nil {
		observer.owner = &status.WorkerDetail{Mode: detail.Mode, Version: detail.Version, NodeName: detail.NodeName}
	}
	observer.lastAlive = detail.UptimeSec
	return true
}

func runControlCenterWorkerMonitor(ctx context.Context, stateDir string, workerPID int, request sessionStatusRequester) error {
	ccWorkerMu.Lock()
	ccSharedWorkerMonitor = true
	ccSharedWorkerDetail = nil
	ccSharedWorkerObservedAt = time.Time{}
	ccWorkerMu.Unlock()
	defer resetControlCenterWorkerDetail()

	publish := func(detail *status.WorkerDetail, observedAt time.Time) {
		ccWorkerMu.Lock()
		defer ccWorkerMu.Unlock()
		ccSharedWorkerDetail, ccSharedWorkerObservedAt = detail, observedAt
	}
	wait := func(ctx context.Context) bool {
		timer := time.NewTimer(controlCenterWorkerDetailInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	return observeControlCenterWorker(ctx, stateDir, workerPID, request, publish, wait, time.Now)
}

// Dependencies make cancellation, sequential requests and stale clearing
// verifiable with fake transports and ticks, without a socket or real timer.
func observeControlCenterWorker(ctx context.Context, stateDir string, workerPID int, request sessionStatusRequester,
	publish func(*status.WorkerDetail, time.Time), wait func(context.Context) bool, now func() time.Time) error {
	observer := sessionWorkerDetailObserver{pid: workerPID}
	publish(nil, time.Time{})
	defer publish(nil, time.Time{})
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !observer.changed {
			detail, err := loadSessionWorkerDetail(ctx, stateDir, request)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || !observer.accept(detail) {
				publish(nil, time.Time{})
			} else {
				publish(detail, now())
			}
		}
		if !wait(ctx) {
			return ctx.Err()
		}
	}
}
