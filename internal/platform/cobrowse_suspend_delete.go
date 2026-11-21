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

const suspendDeleteLimit = 5 * time.Second

// Legacy Stop is not a discard/recovery operation for a retained capture.
// Re-read after exclusion wait: an intent may have committed while teardown
// waited. Unknown metadata also withholds deletion. This only protects the
// registry entry; it neither authorizes stopping resources nor proves teardown.
// Like the other legacy writers, exclusion is in-process, not a process lease.
func deleteLegacyBrowserRecord(ctx context.Context, dir, id string) error {
	fail := func() error { return errors.New("browser lifecycle metadata retained; recovery is required") }
	if ctx == nil || ctx.Err() != nil || !sessionIDPattern.MatchString(id) || len(id) > 64 {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, suspendDeleteLimit)
	defer cancel()
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil // Never create a missing registry just to remove an entry.
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
	if os.IsNotExist(err) && unchanged() {
		return nil
	}
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
	if readErr != nil || closeErr != nil || len(data) > maxSuspendRegistryRecordBytes || json.Unmarshal(data, &rec) != nil ||
		rec.ID != id || rec.Version != sessionRecordVersion || retainedSuspendRecord(rec) ||
		(rec.State != RecordLive && rec.State != RecordExpired) {
		return fail()
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(entry, current) || !unchanged() || root.Remove(name) != nil {
		return fail()
	}
	return nil
}
