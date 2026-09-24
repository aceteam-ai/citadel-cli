// Package finetunesafety defines the on-node durable hold shared by the
// fine-tune worker and startup reservation reconciliation.
package finetunesafety

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const HoldFile = "active.hold"
const lockFile = "reservation.lock"

const OCRService = "unlimited-ocr"
const OllamaService = "ollama"

// CouldEvict names the two serving modules fine-tune may stop. Even if one
// was not running (and therefore has no job tag), a local start while training
// is active would defeat the exclusive-mode window.
func CouldEvict(name string) bool {
	return name == OCRService || name == OllamaService
}

func Dir(configDir string) string {
	return filepath.Join(configDir, "finetune", "safety")
}

func Path(configDir string) string {
	return filepath.Join(Dir(configDir), HoldFile)
}

// WithExclusive serializes hold creation with the entire generic reservation
// release (including service restart). The advisory lock file is never
// removed: replacing its inode would permit two concurrent holders. A crash
// releases the OS lock while the separate active.hold remains durable.
func WithExclusive(safetyDir string, fn func() error) (err error) {
	if err := os.MkdirAll(safetyDir, 0700); err != nil {
		return fmt.Errorf("create fine-tune safety directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(safetyDir, lockFile), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("open fine-tune reservation lock: %w", err)
	}
	if err := lockFileExclusive(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("fine-tune reservation operation already in progress: %w", err)
	}
	defer func() {
		err = errors.Join(err, unlockFileExclusive(f), f.Close())
	}()
	return fn()
}

// RequireAbsent is the generic reservation-release/startup gate. It fails
// closed for any hold, including malformed or inaccessible entries.
func RequireAbsent(configDir string) error {
	if _, err := os.Lstat(Path(configDir)); err == nil {
		return errors.New("fine-tune safety hold remains; verified cleanup required before restoring services")
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return fmt.Errorf("cannot check fine-tune safety hold: %w", err)
	}
}

// RequireOwned is used only by the fine-tune worker after it has confirmed
// that its named trainer is absent. The hold must still belong to that job;
// a generic or manual release must never use this narrower cleanup path.
func RequireOwned(configDir, jobID string) error {
	path := Path(configDir)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot check fine-tune safety hold: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("fine-tune safety hold is not a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read fine-tune safety hold: %w", err)
	}
	if strings.TrimSpace(string(contents)) != jobID {
		return errors.New("fine-tune safety hold belongs to another job")
	}
	return nil
}

// HeldJobID returns the active fine-tune owner. A malformed or unreadable
// hold fails closed because callers cannot safely distinguish a held tag.
func HeldJobID(configDir string) (string, bool, error) {
	path := Path(configDir)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cannot check fine-tune safety hold: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", false, errors.New("fine-tune safety hold is not a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("cannot read fine-tune safety hold: %w", err)
	}
	id := strings.TrimSpace(string(contents))
	if id == "" || len(id) > 80 || strings.ContainsAny(id, "\r\n\t ") {
		return "", false, errors.New("fine-tune safety hold has an invalid job id")
	}
	return id, true, nil
}
