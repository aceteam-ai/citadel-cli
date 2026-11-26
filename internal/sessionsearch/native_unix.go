//go:build linux || darwin

package sessionsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"golang.org/x/sys/unix"
)

func ownerSupported(uid int) bool {
	return uid >= 0 && uid == os.Getuid() && os.Getuid() == os.Geteuid()
}
func owned(info os.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}
func openOwned(root *os.Root, path string, uid int, dir bool) (*os.File, error) {
	// Root confines resolution; no-follow plus nonblocking prevents leaf links
	// and special files from becoming content readers. Parent links are refused.
	parent := filepath.Dir(path)
	for parent != "." {
		info, err := root.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || !owned(info, uid) {
			return nil, ErrUnavailable
		}
		parent = filepath.Dir(parent)
	}
	flags := os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if dir {
		flags |= unix.O_DIRECTORY
	}
	f, err := root.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !owned(info, uid) || (dir && !info.IsDir()) || (!dir && !info.Mode().IsRegular()) {
		f.Close()
		return nil, ErrUnavailable
	}
	return f, nil
}
func Native(ctx context.Context, opts Options, panes []laneinventory.Pane) (Report, error) {
	uid := os.Getuid()
	if !ownerSupported(uid) {
		return Report{}, ErrUnavailable
	}
	account, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || account.Uid != strconv.Itoa(uid) {
		return Report{}, ErrUnavailable
	}
	return Search(ctx, account.HomeDir, uid, opts, panes)
}
func (s *search) hookName(g *group) {
	if g.Harness != "claude" || g.Location != "" {
		return
	}
	sum := sha256.Sum256([]byte(g.ID))
	path := filepath.Join(".local", "state", "aceteam", "claude-session-start", hex.EncodeToString(sum[:])+".json")
	f, err := openOwned(s.root, path, s.uid, false)
	if err != nil {
		return
	}
	defer f.Close()
	var state struct {
		Name string `json:"name"`
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err == nil && len(raw) <= 4096 && json.Unmarshal(raw, &state) == nil && validName(state.Name) {
		g.Title = safe(state.Name)
	}
}
