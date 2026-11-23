package harnessseat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrSeatDirectory       = errors.New("seat_directory_unsafe")
	ErrSeatDirectoryExists = errors.New("seat_directory_exists")
)

// A filesystem-work budget, not a workspace or vendor capacity limit.
const maxSeatDirectoryDepth = 64

// LocalSeatDirectory pins a private directory owned by the process user. It
// opens directories only, never a vendor credential or configuration file.
// The owner/home must already come from the trusted local owner resolver.
// This is not a native isolation proof, a cross-user privilege drop, or launch
// admission. The local CLI reuses read-only checks for its registry path;
// vendor-directory creation remains unwired.
type LocalSeatDirectory struct {
	mu       sync.Mutex
	root     *os.Root
	identity Identity
}

// PrepareLocalSeatDirectory checks an existing directory when create is false.
// Explicit creation makes missing components 0700 but refuses an existing leaf:
// adding a seat must not silently adopt another login. Nothing is chmodded or
// removed, including newly made parents if a later step refuses.
func PrepareLocalSeatDirectory(i Identity, create bool) (*LocalSeatDirectory, error) {
	root, err := openLocalSeatDirectory(i, create)
	if err != nil {
		return nil, err
	}
	return &LocalSeatDirectory{root: root, identity: i}, nil
}

func openLocalSeatDirectory(i Identity, create bool) (*os.Root, error) {
	if !privateStoreSupported() {
		return nil, ErrStoreUnsupported
	}
	if i.Validate() != nil {
		return nil, ErrSeatDirectory
	}
	before, err := os.Lstat(i.HomeDir)
	if err != nil || !ownedSeatParentInfo(before) {
		return nil, ErrSeatDirectory
	}
	root, err := os.OpenRoot(i.HomeDir)
	if err != nil {
		return nil, ErrSeatDirectory
	}
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	after, err := root.Stat(".")
	if err != nil || !ownedSeatParentInfo(after) || !os.SameFile(before, after) {
		return nil, ErrSeatDirectory
	}
	rel, err := filepath.Rel(i.HomeDir, i.ConfigDir)
	if err != nil {
		return nil, ErrSeatDirectory
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > maxSeatDirectoryDepth {
		return nil, ErrSeatDirectory
	}
	for index, part := range parts {
		leaf := index == len(parts)-1
		created := false
		info, err := root.Lstat(part)
		if os.IsNotExist(err) && create {
			// A competing create is a refusal, not implicit adoption.
			if err := root.Mkdir(part, 0o700); err != nil {
				return nil, ErrSeatDirectory
			}
			created = true
			info, err = root.Lstat(part)
		}
		if err != nil || !ownedSeatParentInfo(info) || (leaf && !privateStoreInfo(info, true)) {
			return nil, ErrSeatDirectory
		}
		if leaf && create && !created {
			return nil, ErrSeatDirectoryExists
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return nil, ErrSeatDirectory
		}
		actual, err := next.Stat(".")
		if err != nil || !ownedSeatParentInfo(actual) || !os.SameFile(info, actual) || (leaf && !privateStoreInfo(actual, true)) {
			next.Close()
			return nil, ErrSeatDirectory
		}
		if err := root.Close(); err != nil {
			next.Close()
			return nil, ErrSeatDirectory
		}
		root = next
	}
	result := root
	root = nil
	return result, nil
}

// Check revalidates both the pinned inode and the current no-symlink pathname.
// It cannot authorize a later path-based subprocess launch: that later slice
// must bind identity and custody for the entire process lifetime separately.
func (d *LocalSeatDirectory) Check() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.root == nil {
		return ErrSeatDirectory
	}
	pinned, err := d.root.Stat(".")
	if err != nil || !privateStoreInfo(pinned, true) {
		return ErrSeatDirectory
	}
	current, err := openLocalSeatDirectory(d.identity, false)
	if err != nil {
		return err
	}
	defer current.Close()
	info, err := current.Stat(".")
	if err != nil || !os.SameFile(pinned, info) {
		return ErrSeatDirectory
	}
	return nil
}

func (d *LocalSeatDirectory) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.root == nil {
		return nil
	}
	err := d.root.Close()
	d.root = nil
	if err != nil {
		return ErrSeatDirectory
	}
	return nil
}
