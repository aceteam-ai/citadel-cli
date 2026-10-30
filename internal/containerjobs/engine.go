package containerjobs

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"time"
)

type ExecResult struct {
	Output              []byte
	ExitCode            int
	Truncated, TimedOut bool
	Message             string
}

type ContainerState struct {
	ID               string
	ExecIDs          []string
	OrgID, VolumeRef string
	Running          bool
}
type VolumeState struct {
	OrgID      string
	QuotaBytes int64
}

// Runtime is privileged and operator-configured. A job cannot supply one.
// Lock is process-safe in the native adapter and spans actual operation exit.
type Runtime interface {
	Lock(context.Context) (func(), error)
	Container(context.Context, string) (ContainerState, error)
	Volume(context.Context, string) (VolumeState, error)
	CreateVolume(context.Context, Request) error
	CreateContainer(context.Context, Request) error
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Exec(context.Context, string, []string) (ExecResult, error)
	OpenVolume(context.Context, string) (*os.Root, error)
	Usage(context.Context, string) (int64, int64, error)
	DestroyContainer(context.Context, string) error
	DestroyVolume(context.Context, string) error
}
type Transport interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string, int64) ([]byte, error)
}
type Engine struct {
	Authorize func() bool
	Runtime   Runtime
	Transport Transport
}

func (e *Engine) Run(ctx context.Context, kind string, r Request) (Result, error) {
	out := Result{ContainerRef: r.ContainerRef, VolumeRef: r.VolumeRef}
	if e == nil || e.Runtime == nil || ctx.Err() != nil {
		return out, ErrUnavailable
	}
	unlock, err := e.Runtime.Lock(ctx)
	if err != nil {
		return out, ErrUnavailable
	}
	defer unlock()
	if ctx.Err() != nil {
		return out, ErrUnavailable
	}
	if e.Authorize != nil && !e.Authorize() {
		return out, ErrOwnership
	}
	rt := e.Runtime
	// Import creates only a new volume; runtime creation refuses existing refs.
	if kind == Import {
		return e.importVolume(ctx, r)
	}
	if kind == Create {
		volume, lookupErr := rt.Volume(ctx, r.VolumeRef)
		if errors.Is(lookupErr, ErrNotFound) {
			if err = rt.CreateVolume(ctx, r); err != nil {
				return out, safeError(err)
			}
		} else if lookupErr != nil {
			return out, ErrUnavailable
		} else if volume.OrgID != r.OrgID || volume.QuotaBytes != r.QuotaBytes {
			return out, ErrOwnership
		}
		if err = rt.CreateContainer(ctx, r); err != nil {
			return out, ErrUnavailable
		}
		out.QuotaBytes = r.QuotaBytes
		if ctx.Err() != nil {
			return out, ErrUnavailable
		}
		return out, nil
	}
	ownedVolume := r.VolumeRef
	if r.ContainerRef != "" {
		c, er := rt.Container(ctx, r.ContainerRef)
		if er != nil {
			return out, ErrUnavailable
		}
		if c.OrgID != r.OrgID || r.VolumeRef != "" && c.VolumeRef != r.VolumeRef {
			return out, ErrOwnership
		}
		out.Running = boolPointer(c.Running)
		ownedVolume = c.VolumeRef
		out.VolumeRef = ownedVolume
	}
	if ownedVolume != "" {
		v, er := rt.Volume(ctx, ownedVolume)
		if er != nil {
			return out, ErrUnavailable
		}
		if v.OrgID != r.OrgID {
			return out, ErrOwnership
		}
		out.QuotaBytes = v.QuotaBytes
	}
	switch kind {
	case Start:
		err = rt.Start(ctx, r.ContainerRef)
		out.Running = boolPointer(err == nil)
	case Stop:
		err = rt.Stop(ctx, r.ContainerRef)
		out.Running = boolPointer(err != nil)
	case Exec:
		c, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutSeconds)*time.Second)
		defer cancel()
		var execution ExecResult
		execution, err = rt.Exec(c, r.ContainerRef, r.Argv)
		if len(execution.Output) > OutputLimit {
			return out, ErrBounds
		}
		out.ExitCode, out.Truncated = execution.ExitCode, execution.Truncated
		out.ExecTimedOut = execution.TimedOut
		out.Running = nil // Exec exit is not a fresh container-state observation.
		out.Message = execution.Message
		if execution.TimedOut {
			out.Running = boolPointer(false)
			out.Message = "exec timed out; container stopped, files kept, it restarts on next use"
		}
		out.OutputB64 = base64.StdEncoding.EncodeToString(execution.Output)
	case FileRead, FileWrite, FileList:
		var root *os.Root
		root, err = rt.OpenVolume(ctx, r.VolumeRef)
		if err == nil {
			defer root.Close()
			out, err = files(ctx, root, kind, r, out)
		}
	case Usage:
		out.UsedBytes, out.QuotaBytes, err = rt.Usage(ctx, r.VolumeRef)
	case Export:
		if out.Running == nil || *out.Running {
			return out, ErrUnavailable
		}
		return e.exportVolume(ctx, r, out)
	case Destroy:
		if r.ContainerRef != "" {
			err = rt.DestroyContainer(ctx, r.ContainerRef)
		}
		if err == nil && r.PurgeVolume {
			err = rt.DestroyVolume(ctx, r.VolumeRef)
		}
	default:
		return out, ErrInvalid
	}
	if err != nil || ctx.Err() != nil {
		return out, safeError(err)
	}
	return out, nil
}

func safeError(err error) error {
	if errors.Is(err, ErrBounds) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrOwnership) {
		return err
	}
	return ErrUnavailable
}

func boolPointer(value bool) *bool { return &value }
