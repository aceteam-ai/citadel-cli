package platform

// Unwired crash-orphan cleanup. The eventual manager must exclude legacy
// registry writers for this entire call. No profile, process or timer is used.

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const maxSuspendOrphanRemovals = 16

type suspendOrphanSweepResult struct {
	Scanned, Attempted, Removed, Failed int
	Cursor                              string
}

// Only names produced by this store are eligible. Unknown entries refuse the
// complete scan before deletion, rather than being mistaken for owned debris.
func suspendPayloadName(name string) bool {
	if strings.HasPrefix(name, ".tmp-") {
		suffix := strings.TrimPrefix(name, ".tmp-")
		decoded, err := hex.DecodeString(suffix)
		return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == suffix
	}
	stem := strings.TrimSuffix(name, ".tabs")
	id, generation, ok := strings.Cut(stem, ".")
	if !ok || stem == name || !sessionIDPattern.MatchString(id) || len(id) > 64 {
		return false
	}
	n, err := strconv.ParseUint(generation, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == generation
}

func suspendPayloadReferences(records []sessionRecord) (map[string]bool, error) {
	refs := make(map[string]bool)
	for _, rec := range records {
		if rec.Suspension == nil {
			continue
		}
		name, err := suspendManifestName(rec)
		if err != nil {
			return nil, errors.New("uncertain retained browser capture")
		}
		refs[name] = true
	}
	return refs, nil
}

// A finite pass fully validates both bounded directories before any removal.
// Unknown, non-private, symlinked, oversized or changed objects refuse. The
// cursor survives refusal/partial failure, so a bad entry cannot monopolize
// later passes. Removed counts only flushed removals; an uncertain unlink is
// reconciled by the next pass's initial directory flush, even if now empty.
func (s *suspendIntentStore) sweepOrphanPayloads(ctx context.Context, cursor string) (suspendOrphanSweepResult, error) {
	result := suspendOrphanSweepResult{Cursor: cursor}
	fail := func() (suspendOrphanSweepResult, error) {
		return result, errors.New("browser orphan cleanup incomplete")
	}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil {
		return fail()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.payloads
	p.mu.Lock()
	defer p.mu.Unlock()
	check := func() bool { return ctx.Err() == nil && s.private() == nil && p.private() == nil }
	if !check() || p.removeOrphan == nil {
		return fail()
	}
	records, err := s.snapshot(ctx)
	if err != nil {
		return fail()
	}
	refs, err := suspendPayloadReferences(records)
	if err != nil || s.syncDir() != nil || !check() {
		return fail()
	}
	dir, err := p.root.Open(".")
	if err != nil {
		return fail()
	}
	defer dir.Close()
	files := make(map[string]os.FileInfo)
	var names []string
	var used int64
	count := 0
	for {
		entries, readErr := dir.ReadDir(64)
		for _, entry := range entries {
			count++
			if count >= maxSuspendStoreEntries || !check() {
				return fail()
			}
			info, err := p.root.Lstat(entry.Name())
			if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSuspendManifestBytes+4096 ||
				used > maxSuspendedNodeBytes-info.Size() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
				return fail()
			}
			used += info.Size()
			if entry.Name() == suspendStoreLockName {
				locked, err := p.lockFile.Stat()
				if err != nil || !os.SameFile(info, locked) {
					return fail()
				}
				continue
			}
			if !suspendPayloadName(entry.Name()) {
				return fail()
			}
			files[entry.Name()] = info
			names = append(names, entry.Name())
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fail()
		}
	}
	result.Scanned = len(names)
	if p.syncDir() != nil || !check() {
		return fail()
	}
	sort.Strings(names)
	start := sort.SearchStrings(names, cursor)
	if start < len(names) && names[start] == cursor {
		start++
	}
	for i := 0; i < len(names) && result.Attempted < maxSuspendOrphanRemovals; i++ {
		if !check() {
			return fail()
		}
		name := names[(start+i)%len(names)]
		result.Cursor = name
		if refs[name] {
			continue
		}
		result.Attempted++
		// Re-read references after durability awaits and before unlinking.
		// This is defense in depth, not a substitute for manager exclusion.
		current, err := s.snapshot(ctx)
		if err != nil {
			result.Failed++
			return fail()
		}
		fresh, err := suspendPayloadReferences(current)
		if err != nil || fresh[name] || !check() {
			result.Failed++
			return fail()
		}
		info, err := p.root.Lstat(name)
		if err != nil || !os.SameFile(info, files[name]) || info.Size() != files[name].Size() || info.Mode() != files[name].Mode() {
			result.Failed++
			continue
		}
		if p.removeOrphan(name) != nil || p.syncDir() != nil || !check() {
			result.Failed++
			continue
		}
		result.Removed++
	}
	if result.Failed > 0 {
		return fail()
	}
	return result, nil
}
