package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/google/uuid"
)

const huddleOutboxPollInterval = 15 * time.Second
const huddleOutboxMaxRecords = 4
const huddleTerminalResultMaxBytes = 16 << 10
const huddleOutboxMaxBytes = 32 << 10
const huddleOutboxTotalMaxBytes = huddleOutboxMaxRecords * huddleOutboxMaxBytes

type huddleLifecycleDebt struct {
	OrganizationID string `json:"organizationId"`
	ChannelID      string `json:"channelId"`
	CallID         string `json:"callId"`
	AgentID        string `json:"agentId"`
	NodeID         string `json:"nodeId"`
	JobID          string `json:"jobId"`
	AttemptID      string `json:"attemptId"`
	Status         string `json:"status"`
	Detail         string `json:"detail"`
	Result         string `json:"result"`
}

func (h *HuddleJoinHandler) huddleOutboxDir() string {
	if override := strings.TrimSpace(h.outboxDir); override != "" {
		return override
	}
	return filepath.Join(platform.ConfigDir(), "huddle-lifecycle-outbox")
}

func (h *HuddleJoinHandler) persistLifecycleDebt(p huddleJoinParams, status, detail string) (string, error) {
	h.outboxMu.Lock()
	defer h.outboxMu.Unlock()
	dir := h.huddleOutboxDir()
	if err := validateHuddleOutboxDirectory(dir, false); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create huddle lifecycle outbox: %w", err)
	}
	if err := validateHuddleOutboxDirectory(dir, true); err != nil {
		return "", err
	}
	result := p.TerminalResult
	if result == "" {
		result = "{}"
	}
	record := huddleLifecycleDebt{
		OrganizationID: p.OrganizationID, ChannelID: p.ChannelID, CallID: p.CallID,
		AgentID: p.AgentID, NodeID: p.NodeID, JobID: p.JobID, AttemptID: p.AttemptID,
		Status: status, Detail: truncateHuddleDetail(detail),
		Result: result,
	}
	if !canonicalLifecycleDebt(record) {
		return "", fmt.Errorf("invalid huddle lifecycle debt")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if len(raw) > huddleOutboxMaxBytes {
		return "", fmt.Errorf("huddle lifecycle debt exceeds %d bytes", huddleOutboxMaxBytes)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	targetName := p.AttemptID + ".json"
	processingName := p.AttemptID + ".processing"
	count := 0
	totalBytes := int64(0)
	replacedBytes := int64(0)
	sameAttempt := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".processing") {
			continue
		}
		info, statErr := os.Lstat(filepath.Join(dir, name))
		if statErr != nil {
			return "", fmt.Errorf("inspect huddle lifecycle outbox: %w", statErr)
		}
		count++
		totalBytes += info.Size()
		if name == targetName || name == processingName {
			sameAttempt = true
		}
		if name == targetName {
			replacedBytes = info.Size()
		}
	}
	if count >= huddleOutboxMaxRecords && !sameAttempt {
		return "", fmt.Errorf("huddle lifecycle outbox is full (%d records)", huddleOutboxMaxRecords)
	}
	if totalBytes-replacedBytes+int64(len(raw)) > huddleOutboxTotalMaxBytes {
		return "", fmt.Errorf("huddle lifecycle outbox is full (%d bytes)", huddleOutboxTotalMaxBytes)
	}
	target := filepath.Join(dir, targetName)
	tmp, err := os.CreateTemp(dir, ".huddle-debt-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return "", err
	}
	if directory, err := os.Open(dir); err == nil {
		err = directory.Sync()
		_ = directory.Close()
		if err != nil {
			return "", err
		}
	}
	return target, nil
}

func canonicalLifecycleDebt(debt huddleLifecycleDebt) bool {
	canonicalUUID := func(value string) bool {
		parsed, err := uuid.Parse(value)
		return err == nil && parsed.String() == value
	}
	if !canonicalUUID(debt.OrganizationID) || !canonicalUUID(debt.ChannelID) ||
		!canonicalUUID(debt.CallID) || !canonicalUUID(debt.AgentID) ||
		!canonicalUUID(debt.JobID) || !canonicalUUID(debt.AttemptID) {
		return false
	}
	node, err := strconv.ParseUint(debt.NodeID, 10, 64)
	if err != nil || node == 0 || strconv.FormatUint(node, 10) != debt.NodeID {
		return false
	}
	if debt.Status != "completed" && debt.Status != "failed" && debt.Status != "cancelled" {
		return false
	}
	if len([]rune(debt.Detail)) > 400 || len(debt.Result) > huddleTerminalResultMaxBytes {
		return false
	}
	var result map[string]any
	return json.Unmarshal([]byte(debt.Result), &result) == nil && result != nil
}

func quarantineLifecycleDebt(dir string, entry os.DirEntry) {
	quarantine := filepath.Join(dir, "quarantine")
	if os.MkdirAll(quarantine, 0o700) != nil {
		return
	}
	_ = os.Rename(filepath.Join(dir, entry.Name()), filepath.Join(quarantine, entry.Name()))
}

func quarantineProcessingDebt(dir, processing, originalName string) {
	quarantine := filepath.Join(dir, "quarantine")
	if os.MkdirAll(quarantine, 0o700) != nil {
		return
	}
	_ = os.Rename(processing, filepath.Join(quarantine, originalName))
}

func recoverProcessingDebts(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".processing") {
			continue
		}
		processing := filepath.Join(dir, entry.Name())
		target := strings.TrimSuffix(processing, ".processing") + ".json"
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			_ = os.Rename(processing, target)
		} else {
			_ = os.Remove(processing)
		}
	}
}

func removeLifecycleDebt(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if directory, err := os.Open(filepath.Dir(path)); err == nil {
		err = directory.Sync()
		_ = directory.Close()
		return err
	}
	return nil
}

func (h *HuddleJoinHandler) processLifecycleOutbox(ctx context.Context) {
	dir := h.huddleOutboxDir()
	if validateHuddleOutboxDirectory(dir, true) != nil {
		return
	}
	h.outboxMu.Lock()
	recoverProcessingDebts(dir)
	h.outboxMu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	processed := 0
	for _, entry := range entries {
		if ctx.Err() != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if processed >= huddleOutboxMaxRecords {
			break
		}
		processed++
		path := filepath.Join(dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			quarantineLifecycleDebt(dir, entry)
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Size() > huddleOutboxMaxBytes {
			quarantineLifecycleDebt(dir, entry)
			continue
		}
		processing := strings.TrimSuffix(path, ".json") + ".processing"
		h.outboxMu.Lock()
		err = os.Rename(path, processing)
		h.outboxMu.Unlock()
		if err != nil {
			continue
		}
		raw, err := readHuddleOutboxFile(processing, huddleOutboxMaxBytes)
		if err != nil {
			_ = os.Rename(processing, path)
			continue
		}
		var debt huddleLifecycleDebt
		if json.Unmarshal(raw, &debt) != nil || !canonicalLifecycleDebt(debt) || entry.Name() != debt.AttemptID+".json" {
			quarantineProcessingDebt(dir, processing, entry.Name())
			continue
		}
		p := huddleJoinParams{
			OrganizationID: debt.OrganizationID, ChannelID: debt.ChannelID, CallID: debt.CallID,
			AgentID: debt.AgentID, NodeID: debt.NodeID, JobID: debt.JobID, AttemptID: debt.AttemptID,
			TerminalResult: debt.Result,
		}
		apiBase, deviceToken, err := h.resolveDeviceAuth(p)
		if err != nil {
			h.outboxMu.Lock()
			if _, currentErr := os.Lstat(path); errors.Is(currentErr, os.ErrNotExist) {
				_ = os.Rename(processing, path)
			} else {
				_ = os.Remove(processing)
			}
			h.outboxMu.Unlock()
			continue
		}
		prepare := h.prepareTerminalIntent
		if prepare == nil {
			prepare = prepareHuddleTerminalIntent
		}
		prepareCtx, cancel := context.WithTimeout(ctx, huddleTokenHTTPTimeout)
		err = prepare(prepareCtx, apiBase, deviceToken, p, debt.Status, debt.Detail)
		cancel()
		if errors.Is(err, errHuddleAlreadyTerminal) {
			// The coordinator already committed and published this exact
			// attempt.  An ACK-loss/restart replay has nothing left to recover;
			// retaining the record would retry forever and could never improve
			// the durable result.
			_ = removeLifecycleDebt(processing)
			continue
		}
		if err != nil {
			h.outboxMu.Lock()
			if _, currentErr := os.Lstat(path); errors.Is(currentErr, os.ErrNotExist) {
				_ = os.Rename(processing, path)
			} else {
				_ = os.Remove(processing)
			}
			h.outboxMu.Unlock()
			continue
		}
		// PREPARE atomically indexed the lifecycle and enqueued a fresh exact-node
		// cleanup job. From this point the server is the durable recovery owner.
		_ = removeLifecycleDebt(processing)
	}
}

// RunBackground is owned by the worker Runner lifespan. It reconciles the
// credential-free local terminal-intent outbox across process restarts.
func (h *HuddleJoinHandler) RunBackground(ctx context.Context) {
	h.processLifecycleOutbox(ctx)
	ticker := time.NewTicker(huddleOutboxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.processLifecycleOutbox(ctx)
		}
	}
}
