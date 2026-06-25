package reconcile

import "context"

// DesiredStateProvider is the control-plane transport: it fetches this node's
// assigned desired state and reports actual state back. It is authenticated by
// the node's existing device identity in the live implementation.
//
// The LIVE HTTP implementation (talking to the /fabric control plane) is a
// SEPARATE, LATER issue and depends on the control-plane endpoints that DO NOT
// EXIST YET (aceteam-ai/aceteam#4273). This package provides ONLY the interface
// and an in-memory FakeProvider for tests. A thin HTTP stub is provided in
// http_stub.go but is explicitly NOT wired to anything live.
type DesiredStateProvider interface {
	// Fetch returns the node's control-plane-assigned desired state.
	Fetch(ctx context.Context) (DesiredState, error)
	// Report posts the node's observed actual state back to the control plane.
	Report(ctx context.Context, actual ActualState) error
}

// FakeProvider is an in-memory DesiredStateProvider for tests. It serves a
// settable DesiredState and records every reported ActualState. It is also
// useful as a local "static desired state from a file" provider if a node ever
// wants to pin its state without a control plane (out of scope for #353, but
// the type makes that trivial).
type FakeProvider struct {
	// Desired is the state Fetch returns.
	Desired DesiredState
	// FetchErr, if set, is returned by Fetch (to test fetch-failure paths).
	FetchErr error
	// ReportErr, if set, is returned by Report.
	ReportErr error
	// Reported accumulates every ActualState passed to Report, in order.
	Reported []ActualState
}

// Fetch implements DesiredStateProvider.
func (f *FakeProvider) Fetch(ctx context.Context) (DesiredState, error) {
	if f.FetchErr != nil {
		return DesiredState{}, f.FetchErr
	}
	return f.Desired, nil
}

// Report implements DesiredStateProvider.
func (f *FakeProvider) Report(ctx context.Context, actual ActualState) error {
	f.Reported = append(f.Reported, actual)
	return f.ReportErr
}
