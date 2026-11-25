package cmd

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// allowlistRacyWindow is how recently a file must have changed (by ctime or
// mtime) for its stamp to be too coarse to trust: filesystem timestamps tick
// at kernel granularity, so a second rewrite inside the same tick would carry
// an identical stamp. A file this fresh is re-read instead of cached (the
// "racily clean" rule version-control indexes use). Tests set it to zero.
var allowlistRacyWindow = 2 * time.Second

// cachedAllowedLogins wraps an allowlist loader so a request does not re-read
// and re-parse the file each time. The cache key is the file's existence and
// fileStamp (device, inode, mtime, ctime, size), checked with one stat per
// call: a rename-replace, a content rewrite (even one that restores mtime) or a
// permission change all reload. A loader error is never cached (the next call
// retries), so a malformed or unreadable file keeps failing closed. On a
// platform that cannot supply a stamp, and for a file changed within
// allowlistRacyWindow, nothing is cached.
func cachedAllowedLogins(path string, load func() (map[string]bool, error)) func() (map[string]bool, error) {
	var (
		mu     sync.Mutex
		valid  bool
		exists bool
		stamp  fileStamp
		cached map[string]bool
	)
	return func() (map[string]bool, error) {
		fi, statErr := os.Stat(path)
		nowExists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return nil, statErr
		}
		var st fileStamp
		stampOK := true
		if nowExists {
			st, stampOK = statFileStamp(fi)
		}

		mu.Lock()
		defer mu.Unlock()
		if stampOK && valid && exists == nowExists && stamp == st {
			return cached, nil
		}
		m, err := load()
		if err != nil || !stampOK {
			valid = false
			return m, err
		}
		if nowExists && allowlistRacyWindow > 0 {
			now := time.Now().UnixNano()
			w := allowlistRacyWindow.Nanoseconds()
			if now-st.ctime < w || now-st.mtime < w {
				valid = false
				return m, nil
			}
		}
		valid, exists, stamp, cached = true, nowExists, st, m
		return m, nil
	}
}

// platformAllowedLoginsCached is the allowlist loader the gateway's /v1 gate
// uses: platformAllowedLogins behind the stamp-keyed cache. The path is resolved
// once, matching platformAllowedLogins' own location.
func platformAllowedLoginsCached() func() (map[string]bool, error) {
	return cachedAllowedLogins(filepath.Join(platformConfigDir(), "platform-exposure.json"), platformAllowedLogins)
}
