package mediainput

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/coordinatorfetch"
)

// Freezer prepares inactive v1 inputs from workspace and/or inbox. Origin must
// come from trusted converged node configuration, never a job. The future C5
// caller must reserve node-wide holder admission BEFORE calling Freeze and
// provide live predicates. This library does not advertise or authorize tasks.
type Freezer struct {
	WorkspaceDir       string
	Origin             func() string
	FilesEnabled       func() bool
	MediaInputsEnabled func() bool
	// Private per-instance owned-fixture seams; never configured from payloads.
	hooks     coordinatorfetch.Hooks
	afterRead func()
}

func (f Freezer) Freeze(ctx context.Context, contract Contract) (Snapshot, error) {
	if len(contract.inputs) < 1 || len(contract.inputs) > 2 {
		return Snapshot{}, refuse("invalid_schema")
	}
	workspace, inbox := false, false
	for _, input := range contract.inputs {
		switch input.Origin {
		case "workspace":
			workspace = true
		case "inbox":
			inbox = true
		default:
			return Snapshot{}, refuse("invalid_schema")
		}
	}
	gate := func() error {
		if ctx.Err() != nil {
			return refuse("cancelled_or_timeout")
		}
		if f.MediaInputsEnabled == nil || !f.MediaInputsEnabled() {
			return refuse("media_inputs_disabled")
		}
		if workspace && (f.FilesEnabled == nil || !f.FilesEnabled()) {
			return refuse("files_disabled")
		}
		return nil
	}
	if err := gate(); err != nil {
		return Snapshot{}, err
	}
	origin := ""
	if inbox {
		if f.Origin == nil {
			return Snapshot{}, refuse("inbox_unavailable")
		}
		origin = f.Origin()
		if _, err := coordinatorfetch.Origin(origin); err != nil {
			return Snapshot{}, refuse("untrusted_origin")
		}
	}
	var root *os.Root
	if workspace {
		if f.WorkspaceDir == "" {
			return Snapshot{}, refuse("workspace_unavailable")
		}
		var err error
		root, err = os.OpenRoot(f.WorkspaceDir)
		if err != nil {
			return Snapshot{}, refuse("workspace_unavailable")
		}
		defer root.Close()
	}
	snapshot := Snapshot{inputs: make([]frozenInput, 0, len(contract.inputs))}
	for _, input := range contract.inputs {
		if err := gate(); err != nil {
			return Snapshot{}, err
		}
		var frozen frozenInput
		var err error
		if input.Origin == "workspace" {
			frozen, err = (WorkspaceFreezer{afterRead: f.afterRead}).read(ctx, root, input)
		} else {
			frozen, err = f.readInbox(ctx, origin, input)
		}
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.inputs = append(snapshot.inputs, frozen)
	}
	if len(snapshot.inputs) == 2 {
		source, mask := snapshot.inputs[0].metadata, snapshot.inputs[1].metadata
		if source.Width != mask.Width || source.Height != mask.Height {
			return Snapshot{}, refuse("mask_invalid")
		}
	}
	if err := gate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (f Freezer) readInbox(ctx context.Context, origin string, input Descriptor) (frozenInput, error) {
	if input.Size < 1 || input.Size > MaxFileBytes {
		return frozenInput{}, refuse("size_exceeded")
	}
	// Reserve the extra byte for the shared declared-size+1 check, preventing
	// bytes.Buffer capacity doubling on an otherwise full-cap input.
	buffer := bytes.NewBuffer(make([]byte, 0, int(input.Size)+1))
	result, err := coordinatorfetch.Get(ctx, origin, coordinatorfetch.MediaInput, input.Capability.Reveal(), input.Size, buffer, f.hooks)
	if err != nil {
		var refusal *coordinatorfetch.Refusal
		if errors.As(err, &refusal) {
			return frozenInput{}, refuse(refusal.Reason())
		}
		return frozenInput{}, refuse("fetch_failed")
	}
	if result.Bytes != input.Size {
		return frozenInput{}, refuse("size_mismatch")
	}
	if f.afterRead != nil {
		f.afterRead()
	}
	if ctx.Err() != nil {
		return frozenInput{}, refuse("cancelled_or_timeout")
	}
	return freezeImageData(buffer.Bytes(), input)
}
