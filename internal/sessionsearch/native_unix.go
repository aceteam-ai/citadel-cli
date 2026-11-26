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
	"strings"
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
	// Pin each parent directory and compare its identity before descending.
	// A later rename cannot redirect the leaf open through an unchecked parent.
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, ErrUnavailable
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || !owned(info, uid) {
			return nil, ErrUnavailable
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			return nil, err
		}
		pinned, err := next.Stat(".")
		if err != nil || !os.SameFile(info, pinned) || !owned(pinned, uid) {
			next.Close()
			return nil, ErrUnavailable
		}
		defer next.Close()
		current = next
	}
	flags := os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if dir {
		flags |= unix.O_DIRECTORY
	}
	f, err := current.OpenFile(parts[len(parts)-1], flags, 0)
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
	if err != nil || account == nil || account.Uid != strconv.Itoa(uid) {
		return Report{}, ErrUnavailable
	}
	return Search(ctx, account.HomeDir, uid, opts, panes)
}
func (s *search) hookName(g *Hit) {
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
