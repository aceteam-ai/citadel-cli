package housekeeping

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"time"
)

var (
	ErrTreeInput       = errors.New("housekeeping_tree_input_invalid")
	ErrTreeLimit       = errors.New("housekeeping_tree_limit")
	ErrTreeRead        = errors.New("housekeeping_tree_read_failed")
	ErrTreeUnsupported = errors.New("housekeeping_tree_entry_unsupported")
	ErrTreeCanceled    = errors.New("housekeeping_tree_canceled")
)

// TreeLimits are mandatory. They bound retained directory entries, traversal
// depth and aggregate work, not the duration of an individual filesystem call.
type TreeLimits struct {
	MaxEntries int
	MaxDepth   int
}

// TreeInventory contains logical metadata only. It is neither an atomic
// snapshot nor evidence of ownership, mount isolation or safe cleanup. Hard
// links and sparse files make LogicalBytes different from reclaimable space.
type TreeInventory struct {
	Entries       uint64    `json:"entries"`
	RegularFiles  uint64    `json:"regular_files"`
	LogicalBytes  uint64    `json:"logical_bytes"`
	NewestEntryAt time.Time `json:"newest_entry_at"`
}

// Observation deliberately does not establish free space, open-handle, in-use
// or Git state. Passing it straight to Plan always skips cleanup candidates.
// observedAt is a caller assertion, not a verified collection timestamp.
func (t TreeInventory) Observation(observedAt time.Time) Observation {
	return Observation{
		ObservedAt: observedAt, NewestEntryAt: t.NewestEntryAt,
		EstimatedBytes: t.LogicalBytes, OpenHandles: Unknown, InUse: Unknown,
		WorktreeClean: Unknown, FullyPushed: Unknown,
	}
}

// InspectTree inspects only a caller-opened root. It opens directories, never
// file contents, and never writes or removes entries. It retains no entry
// names in the result or errors. The caller retains ownership of root.
//
// Symlinks and non-regular entries are refused when observed. Directory identity
// is checked after opening, but this is not a race-free safety proof. os.Root
// does not exclude mounts. An approved caller must establish appropriate roots,
// ownership and mount boundaries before any future native integration. There
// is currently no production caller, CLI path, root discovery or deletion path.
func InspectTree(ctx context.Context, root *os.Root, limits TreeLimits) (TreeInventory, error) {
	if ctx == nil || root == nil || limits.MaxEntries < 1 || limits.MaxEntries > 1_000_000 || limits.MaxDepth < 1 || limits.MaxDepth > 64 {
		return TreeInventory{}, ErrTreeInput
	}
	var result TreeInventory
	if err := inspectDirectory(ctx, root, limits, 0, &result); err != nil {
		return TreeInventory{}, err
	}
	return result, nil
}

func inspectDirectory(ctx context.Context, root *os.Root, limits TreeLimits, depth int, result *TreeInventory) (err error) {
	if ctx.Err() != nil {
		return ErrTreeCanceled
	}
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() {
		return ErrTreeRead
	}
	if info.ModTime().After(result.NewestEntryAt) {
		result.NewestEntryAt = info.ModTime().UTC()
	}
	dir, err := root.Open(".")
	if err != nil {
		return ErrTreeRead
	}
	defer func() {
		if closeErr := dir.Close(); err == nil && closeErr != nil {
			err = ErrTreeRead
		}
	}()
	for {
		if ctx.Err() != nil {
			return ErrTreeCanceled
		}
		entries, readErr := dir.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			return ErrTreeRead
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return ErrTreeCanceled
			}
			if result.Entries >= uint64(limits.MaxEntries) {
				return ErrTreeLimit
			}
			info, statErr := root.Lstat(entry.Name())
			if statErr != nil {
				return ErrTreeRead
			}
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return ErrTreeUnsupported
			}
			result.Entries++
			if info.ModTime().After(result.NewestEntryAt) {
				result.NewestEntryAt = info.ModTime().UTC()
			}
			if info.IsDir() {
				if depth >= limits.MaxDepth {
					return ErrTreeLimit
				}
				child, openErr := root.OpenRoot(entry.Name())
				if openErr != nil {
					return ErrTreeRead
				}
				opened, statErr := child.Stat(".")
				var walkErr error
				if statErr != nil || !os.SameFile(info, opened) {
					walkErr = ErrTreeRead
				} else {
					walkErr = inspectDirectory(ctx, child, limits, depth+1, result)
				}
				closeErr := child.Close()
				if walkErr != nil {
					return walkErr
				}
				if closeErr != nil {
					return ErrTreeRead
				}
			} else {
				if info.Size() < 0 || uint64(info.Size()) > math.MaxUint64-result.LogicalBytes {
					return ErrTreeLimit
				}
				result.RegularFiles++
				result.LogicalBytes += uint64(info.Size())
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}
