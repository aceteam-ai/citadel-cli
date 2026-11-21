package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"time"
)

const suspendStartFenceLimit = 5 * time.Second

// withNewSessionRegistryFence prevents a legacy start from replacing retained
// suspension metadata. The callback reserves only an in-memory slot, never
// launches, writes the registry or re-enters its gate. This is in-process
// exclusion, not a durable launch reservation or a cross-process start lease.
// Local managers never inspect the node registry. No capture/profile is opened.
func (m *CobrowseSessionManager) withNewSessionRegistryFence(ctx context.Context, id string, reserve func(context.Context, string) error) error {
	fail := func() error {
		return errors.New("browser session identity is retained or unavailable; reopen or discard it first")
	}
	if m == nil || ctx == nil || ctx.Err() != nil || reserve == nil || !sessionIDPattern.MatchString(id) || len(id) > 64 {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, suspendStartFenceLimit)
	defer cancel()
	if !m.useResolver {
		return reserve(ctx, "")
	}
	dir := m.registryDir()
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if ctx.Err() != nil {
			return fail()
		}
		return reserve(ctx, dir) // No registry is created by this read-only check.
	}
	if err != nil || !info.IsDir() || validateCobrowseBaseDirPerms(dir, info) != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return fail()
	}
	release, err := acquireBrowserRegistryGate(ctx, dir)
	if err != nil {
		return fail()
	}
	defer release()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fail()
	}
	defer root.Close()
	unchanged := func() bool {
		opened, e1 := root.Stat(".")
		named, e2 := os.Lstat(dir)
		return ctx.Err() == nil && e1 == nil && e2 == nil && os.SameFile(info, opened) && os.SameFile(info, named) &&
			validateCobrowseBaseDirPerms(dir, opened) == nil &&
			(runtime.GOOS == "windows" || opened.Mode().Perm()&0o077 == 0)
	}
	if !unchanged() {
		return fail()
	}
	name := id + ".json"
	entry, err := root.Lstat(name)
	if !os.IsNotExist(err) {
		if err != nil || !entry.Mode().IsRegular() || entry.Size() <= 0 || entry.Size() > maxSuspendRegistryRecordBytes ||
			(runtime.GOOS != "windows" && entry.Mode().Perm()&0o077 != 0) {
			return fail()
		}
		f, err := root.Open(name)
		if err != nil {
			return fail()
		}
		opened, statErr := f.Stat()
		if statErr != nil || !os.SameFile(entry, opened) {
			_ = f.Close()
			return fail()
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxSuspendRegistryRecordBytes+1))
		closeErr := f.Close()
		var rec sessionRecord
		if readErr != nil || closeErr != nil || len(data) > maxSuspendRegistryRecordBytes ||
			json.Unmarshal(data, &rec) != nil || rec.ID != id || rec.Version != sessionRecordVersion ||
			rec.Suspension != nil || rec.EndedReason == "suspend_expired" {
			return fail()
		}
		switch rec.State {
		case RecordLive:
			// Existing live-record adoption/retry behavior is unchanged.
		case RecordExpired:
		default:
			// Includes suspending, suspended, resuming and unknown states.
			return fail()
		}
	}
	if !unchanged() {
		return fail()
	}
	// Once reserve commits, do not turn a later cancellation into an error
	// that leaks a successfully reserved slot. No awaited effect follows it.
	return reserve(ctx, dir)
}
