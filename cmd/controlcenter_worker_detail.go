package cmd

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
)

const controlCenterWorkerDetailInterval = 5 * time.Second

// These observations belong to the monitor, never to this process's consume
// loop. ccWorkerMu protects these values and the existing worker state.
var (
	ccSharedWorkerMonitor      bool
	ccSharedWorkerOwnerChanged bool
	ccSharedWorkerDetail       *status.WorkerDetail
	ccSharedWorkerObservedAt   time.Time
)

func resetControlCenterWorkerDetail() {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	ccSharedWorkerMonitor = false
	ccSharedWorkerOwnerChanged = false
	ccSharedWorkerDetail = nil
	ccSharedWorkerObservedAt = time.Time{}
}

func controlCenterWorkerDetailSnapshot() (bool, *status.WorkerDetail, time.Time, bool) {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	if ccSharedWorkerDetail == nil {
		return ccSharedWorkerMonitor, nil, time.Time{}, ccSharedWorkerOwnerChanged
	}
	copy, err := status.CloneWorkerDetail(*ccSharedWorkerDetail)
	if err != nil {
		return ccSharedWorkerMonitor, nil, time.Time{}, ccSharedWorkerOwnerChanged
	}
	return ccSharedWorkerMonitor, &copy, ccSharedWorkerObservedAt, ccSharedWorkerOwnerChanged
}

func applyControlCenterWorkerDetail(data *controlcenter.StatusData) {
	data.SharedWorker, data.WorkerDetail, data.WorkerDetailObservedAt, data.WorkerOwnerChanged = controlCenterWorkerDetailSnapshot()
	if data.SharedWorker {
		// The local monitor goroutine is not the selected worker's consume loop.
		// Missing detail stays unavailable, never a green local running flag.
		data.WorkerRunning = !data.WorkerOwnerChanged && data.WorkerDetail != nil && data.WorkerDetail.Worker != nil && data.WorkerDetail.Worker.Consuming
		data.WorkerQueue = ""
	}
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
		detail.UptimeSec < observer.lastAlive) {
		observer.changed = true
		return false
	}
	// Node name, mesh IP and connectivity may change during reconnect or rename
	// without replacing this worker. They are display data, not identity proofs.
	if observer.owner == nil {
		observer.owner = &status.WorkerDetail{Mode: detail.Mode, Version: detail.Version}
	}
	observer.lastAlive = detail.UptimeSec
	return true
}

func runControlCenterWorkerMonitor(ctx context.Context, stateDir string, workerPID int, request sessionStatusRequester) error {
	ccWorkerMu.Lock()
	ccSharedWorkerMonitor = true
	ccSharedWorkerOwnerChanged = false
	ccSharedWorkerDetail = nil
	ccSharedWorkerObservedAt = time.Time{}
	activityFn := ccActivityFn
	ccWorkerMu.Unlock()
	defer resetControlCenterWorkerDetail()

	publish := func(detail *status.WorkerDetail, observedAt time.Time, ownerChanged bool) {
		ccWorkerMu.Lock()
		defer ccWorkerMu.Unlock()
		ccSharedWorkerDetail, ccSharedWorkerObservedAt = detail, observedAt
		ccSharedWorkerOwnerChanged = ownerChanged
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
	ownerChanged := func() {
		if activityFn != nil {
			activityFn("warning", controlcenter.SharedWorkerOwnerChangedMessage)
		}
	}
	return observeControlCenterWorker(ctx, stateDir, workerPID, request, publish, wait, time.Now, ownerChanged)
}

// Dependencies make cancellation, sequential requests and stale clearing
// verifiable with fake transports and ticks, without a socket or real timer.
func observeControlCenterWorker(ctx context.Context, stateDir string, workerPID int, request sessionStatusRequester,
	publish func(*status.WorkerDetail, time.Time, bool), wait func(context.Context) bool, now func() time.Time, ownerChanged func()) error {
	observer := sessionWorkerDetailObserver{pid: workerPID}
	publish(nil, time.Time{}, false)
	defer publish(nil, time.Time{}, false)
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
				publish(nil, time.Time{}, observer.changed)
				if observer.changed && ownerChanged != nil {
					ownerChanged()
				}
			} else {
				publish(detail, now(), false)
			}
		}
		if !wait(ctx) {
			return ctx.Err()
		}
	}
}
