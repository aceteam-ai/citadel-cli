package laneinventory

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxThreadFDs = 256
const maxThreadProcesses = 256

var rolloutName = regexp.MustCompile(`^rollout-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-([0-9a-fA-F-]{36})\.jsonl$`)
var sessionDate = regexp.MustCompile(`^[0-9]{4}/[0-9]{2}/[0-9]{2}$`)

// threadFromPath projects only an identifier from a default transcript root.
// It does not open the link target or read transcript contents.
func threadFromPath(home, harness, target string) string {
	if !filepath.IsAbs(home) || !filepath.IsAbs(target) || filepath.Clean(target) != target || len(target) > 4096 {
		return ""
	}
	var root string
	switch harness {
	case "claude":
		root = filepath.Join(home, ".claude", "projects")
	case "codex":
		root = filepath.Join(home, ".codex", "sessions")
	default:
		return ""
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	var id string
	if harness == "claude" && len(parts) == 2 {
		id = strings.TrimSuffix(parts[1], ".jsonl")
		if id == parts[1] {
			return ""
		}
	} else if harness == "codex" && len(parts) == 4 && sessionDate.MatchString(strings.Join(parts[:3], "/")) {
		match := rolloutName.FindStringSubmatch(parts[3])
		if len(match) == 2 {
			id = match[1]
		}
	}
	if !uuid.MatchString(id) {
		return ""
	}
	return strings.ToLower(id)
}

func readThreadFDs(ctx context.Context, procRoot, home string, pid int32, harness string) (string, error) {
	if ctx.Err() != nil || pid <= 0 {
		return "", ErrUnavailable
	}
	dirPath := filepath.Join(procRoot, strconv.Itoa(int(pid)), "fd")
	dir, err := os.Open(dirPath)
	if err != nil {
		return "", ErrUnavailable
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxThreadFDs + 1)
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > maxThreadFDs {
		return "", ErrUnavailable
	}
	id := ""
	for _, entry := range entries {
		if ctx.Err() != nil {
			return "", ErrUnavailable
		}
		fd, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil || strconv.FormatUint(fd, 10) != entry.Name() {
			continue
		}
		target, err := os.Readlink(filepath.Join(dirPath, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", ErrUnavailable
		}
		candidate := threadFromPath(home, harness, target)
		if candidate == "" {
			continue
		}
		if id != "" && id != candidate {
			return "", ErrUnavailable
		}
		id = candidate
	}
	return id, nil
}

// nativeThreadID shares the before/after UID and process-birth check used by
// argv observation. A replaced process cannot inherit descriptor metadata.
func nativeThreadID(ctx context.Context, uid int32, expected Process, home, procRoot, harness string, identity func(context.Context) ([]int32, int64, error)) (string, error) {
	if harness != "claude" && harness != "codex" {
		return "", nil
	}
	ids, err := nativeArgv(ctx, uid, expected, identity, func(ctx context.Context) ([]string, error) {
		id, err := readThreadFDs(ctx, procRoot, home, expected.PID, harness)
		return []string{id}, err
	})
	if err != nil || len(ids) != 1 {
		return "", ErrUnavailable
	}
	return ids[0], nil
}
