package cmd

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// cachedAllowedLogins wraps an allowlist loader so a request does not re-read
// and re-parse the file each time. The cache key is the file's existence, mtime
// and size, checked with one stat per call: any change reloads, and a loader
// error is never cached (the next call retries), so a malformed file keeps
// failing closed until it is fixed.
func cachedAllowedLogins(path string, load func() (map[string]bool, error)) func() (map[string]bool, error) {
	var (
		mu      sync.Mutex
		valid   bool
		exists  bool
		modTime time.Time
		size    int64
		cached  map[string]bool
	)
	return func() (map[string]bool, error) {
		fi, statErr := os.Stat(path)
		nowExists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return nil, statErr
		}
		var mt time.Time
		var sz int64
		if nowExists {
			mt, sz = fi.ModTime(), fi.Size()
		}

		mu.Lock()
		defer mu.Unlock()
		if valid && exists == nowExists && modTime.Equal(mt) && size == sz {
			return cached, nil
		}
		m, err := load()
		if err != nil {
			valid = false
			return nil, err
		}
		valid, exists, modTime, size, cached = true, nowExists, mt, sz, m
		return m, nil
	}
}

// platformAllowedLoginsCached is the allowlist loader the gateway's /v1 gate
// uses: platformAllowedLogins behind the stat-keyed cache. The path is resolved
// once, matching platformAllowedLogins' own location.
func platformAllowedLoginsCached() func() (map[string]bool, error) {
	return cachedAllowedLogins(filepath.Join(platformConfigDir(), "platform-exposure.json"), platformAllowedLogins)
}
