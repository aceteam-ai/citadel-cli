package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileWriteIfUnchangedHandler saves text only when the node still has the
// version the editor loaded. A distinct job type makes older nodes fail closed
// instead of ignoring the preconditions on FILE_WRITE.
type FileWriteIfUnchangedHandler struct{ WorkspaceDir string }

type conditionalSaveLock struct {
	identity os.FileInfo
	mu       sync.Mutex
	users    int
}

var conditionalSaveLocks struct {
	sync.Mutex
	entries []*conditionalSaveLock
}

// lockConditionalSave serializes all conditional writes to the same opened
// file, including writes through hard links or different handler instances.
// Entries live only while a writer holds or waits on the file lock.
func lockConditionalSave(identity os.FileInfo) func() {
	conditionalSaveLocks.Lock()
	var entry *conditionalSaveLock
	for _, candidate := range conditionalSaveLocks.entries {
		if os.SameFile(identity, candidate.identity) {
			entry = candidate
			break
		}
	}
	if entry == nil {
		entry = &conditionalSaveLock{identity: identity}
		conditionalSaveLocks.entries = append(conditionalSaveLocks.entries, entry)
	}
	entry.users++
	conditionalSaveLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		conditionalSaveLocks.Lock()
		entry.users--
		if entry.users == 0 {
			for index, candidate := range conditionalSaveLocks.entries {
				if candidate == entry {
					conditionalSaveLocks.entries = append(conditionalSaveLocks.entries[:index], conditionalSaveLocks.entries[index+1:]...)
					break
				}
			}
		}
		conditionalSaveLocks.Unlock()
	}
}

func (h *FileWriteIfUnchangedHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path := job.Payload["path"]
	content, hasContent := job.Payload["content"]
	expectedSize, sizeErr := strconv.ParseInt(job.Payload["expected_size"], 10, 64)
	expectedMtime, timeErr := time.Parse(time.RFC3339Nano, job.Payload["expected_modified_at"])
	expectedHash := job.Payload["expected_sha256"]
	if path == "" || !hasContent || sizeErr != nil || expectedSize < 0 || timeErr != nil || len(expectedHash) != 64 {
		return nil, fmt.Errorf("invalid conditional file write payload")
	}
	if _, err := hex.DecodeString(expectedHash); err != nil {
		return nil, fmt.Errorf("invalid conditional file write digest")
	}
	validated, err := ValidatePath(h.WorkspaceDir, path)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}
	// Keep the same descriptor for verification and write. Reopening by path
	// after validation would let a concurrent symlink swap redirect the write.
	file, err := os.OpenFile(validated, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open file for conditional save: %w", err)
	}
	defer file.Close()
	identity, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot stat file for conditional save: %w", err)
	}
	unlock := lockConditionalSave(identity)
	defer unlock()
	// Another editor may have completed while this request waited for the
	// file lock. Validate the open descriptor again inside the critical section.
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot stat file for conditional save: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize || !info.ModTime().UTC().Equal(expectedMtime.UTC()) {
		return nil, fmt.Errorf("file changed since load")
	}
	// Bound this read independently of a maliciously large expected_size.
	if expectedSize > 5*1024*1024 || len(content) > 5*1024*1024 {
		return nil, fmt.Errorf("conditional file write exceeds 5 MB")
	}
	old := make([]byte, expectedSize)
	if n, err := file.ReadAt(old, 0); err != nil || int64(n) != expectedSize {
		return nil, fmt.Errorf("file changed since load")
	}
	hash := sha256.Sum256(old)
	if hex.EncodeToString(hash[:]) != expectedHash {
		return nil, fmt.Errorf("file changed since load")
	}
	// Recheck the path immediately before the write, including symlink changes.
	if latest, err := ValidatePath(h.WorkspaceDir, path); err != nil || latest != validated {
		return nil, fmt.Errorf("file changed since load")
	}
	current, err := os.Stat(validated)
	if err != nil || !os.SameFile(info, current) || current.Size() != expectedSize || !current.ModTime().UTC().Equal(expectedMtime.UTC()) {
		return nil, fmt.Errorf("file changed since load")
	}
	ctx.Log("info", "     - [Job %s] FILE_WRITE_IF_UNCHANGED %s (%d bytes)", job.ID, validated, len(content))
	if err := writeConditionalContent(file, []byte(content)); err != nil {
		return nil, fmt.Errorf("failed to save file: %w", err)
	}
	return json.Marshal(map[string]any{"path": validated, "bytes_written": len(content)})
}

func writeConditionalContent(file *os.File, content []byte) error {
	if _, err := file.WriteAt(content, 0); err != nil {
		return err
	}
	return file.Truncate(int64(len(content)))
}

var _ JobHandler = (*FileWriteIfUnchangedHandler)(nil)
