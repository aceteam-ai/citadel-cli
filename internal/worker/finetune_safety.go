package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
)

const fineTuneSafetyHold = finetunesafety.HoldFile

func (h *FineTuneHandler) safetyHoldPath() (string, error) {
	if h.cfg.SafetyDir == "" {
		return "", errors.New("fine-tune safety directory is not configured")
	}
	return filepath.Join(h.cfg.SafetyDir, fineTuneSafetyHold), nil
}

// The hold is created before reservation or container launch and removed only
// after verified cleanup and canonical terminal persistence. It survives a
// worker crash. Recovery is deliberately manual: verify the named training
// container is absent, restore any job-tagged services, reconcile job status,
// then remove active.hold. A later demand is never an implicit recovery step.
func (h *FineTuneHandler) armSafetyHold(jobID string) error {
	path, err := h.safetyHoldPath()
	if err != nil {
		return err
	}
	return finetunesafety.WithExclusive(h.cfg.SafetyDir, func() error {
		if err := syncFineTuneSafetyDir(filepath.Dir(h.cfg.SafetyDir)); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(jobID + "\n"); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return syncFineTuneSafetyDir(h.cfg.SafetyDir)
	})
}

func (h *FineTuneHandler) clearSafetyHold(jobID string) error {
	path, err := h.safetyHoldPath()
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("fine-tune safety hold is not a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(contents)) != jobID {
		return fmt.Errorf("fine-tune safety hold belongs to another job")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncFineTuneSafetyDir(h.cfg.SafetyDir)
}

func (h *FineTuneHandler) ensureSafeForDemand() error {
	path, err := h.safetyHoldPath()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("fine-tune safety hold remains; demand admission refused")
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return fmt.Errorf("fine-tune safety hold cannot be checked: %w", err)
	}
}

func syncFineTuneSafetyDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
