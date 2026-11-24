// Package housekeeping evaluates advisory prune policy. It never discovers
// paths, reads file contents, shells out, changes configuration or deletes data.
// Supplied observations are declarations, not cleanup authorization.
package housekeeping

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"sort"
	"time"
)

const (
	MaxInputBytes                 = 1 << 20
	MaxTargets                    = 256
	DefaultKeepFreeBytes   uint64 = 20 * 1024 * 1024 * 1024
	DefaultMinimumAgeHours        = 7 * 24
	maxObservationAge             = 5 * time.Minute
)

var ErrInput = errors.New("housekeeping_input_invalid")
var targetID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type State string

const (
	Unknown State = "unknown"
	Yes     State = "yes"
	No      State = "no"
)

// Input deliberately contains no paths, commands, usernames or remote identity.
// A future native inventory adapter must establish its own trustworthy mapping.
type Input struct {
	Version int      `json:"version"`
	Targets []Target `json:"targets"`
}

type Target struct {
	ID              string       `json:"id"`
	Kind            string       `json:"kind"`
	MinimumAgeHours int          `json:"minimum_age_hours,omitempty"`
	KeepFreeBytes   uint64       `json:"keep_free_bytes,omitempty"`
	Observation     *Observation `json:"observation,omitempty"`
}

type Observation struct {
	ObservedAt     time.Time `json:"observed_at"`
	NewestEntryAt  time.Time `json:"newest_entry_at"`
	EstimatedBytes uint64    `json:"estimated_bytes"`
	AvailableBytes *uint64   `json:"available_bytes"`
	OpenHandles    State     `json:"open_handles"`
	InUse          State     `json:"in_use"`
	WorktreeClean  State     `json:"worktree_clean,omitempty"`
	FullyPushed    State     `json:"fully_pushed,omitempty"`
}

type TargetReceipt struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Decision        string `json:"decision"`
	Reason          string `json:"reason"`
	EstimatedBytes  uint64 `json:"estimated_bytes"`
	KeepFreeBytes   uint64 `json:"keep_free_bytes"`
	MinimumAgeHours int    `json:"minimum_age_hours"`
}

type Receipt struct {
	Version                 int             `json:"version"`
	Mode                    string          `json:"mode"`
	Source                  string          `json:"source"`
	CreatedAt               time.Time       `json:"created_at"`
	DeletionEnabled         bool            `json:"deletion_enabled"`
	FreedBytes              uint64          `json:"freed_bytes"`
	EstimatedCandidateBytes uint64          `json:"estimated_candidate_bytes"`
	Targets                 []TargetReceipt `json:"targets"`
}

// ReadInput accepts one bounded JSON document, not arbitrary paths or shell
// commands. Parser and underlying I/O errors never echo supplied content.
func ReadInput(r io.Reader) (Input, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxInputBytes+1))
	if err != nil || len(data) > MaxInputBytes {
		return Input{}, ErrInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input Input
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || validate(input) != nil {
		return Input{}, ErrInput
	}
	return input, nil
}

func validState(s State) bool { return s == "" || s == Unknown || s == Yes || s == No }

func protected(kind string) bool {
	switch kind {
	case "model_cache", "module_store", "data_directory", "container_images":
		return true
	}
	return false
}

func validate(input Input) error {
	if input.Version != 1 || input.Targets == nil || len(input.Targets) > MaxTargets {
		return ErrInput
	}
	seen := make(map[string]bool)
	for _, t := range input.Targets {
		if !targetID.MatchString(t.ID) || seen[t.ID] || t.MinimumAgeHours < 0 || t.MinimumAgeHours > 365*24 {
			return ErrInput
		}
		seen[t.ID] = true
		switch t.Kind {
		case "go_build_cache", "pnpm_store", "npm_cache", "uv_cache", "gradle_cache", "docker_builder_cache", "scratch", "worktree":
		default:
			if !protected(t.Kind) {
				return ErrInput
			}
		}
		if o := t.Observation; o != nil && (!validState(o.OpenHandles) || !validState(o.InUse) || !validState(o.WorktreeClean) || !validState(o.FullyPushed)) {
			return ErrInput
		}
	}
	return nil
}

// Plan is pure and fail-closed for unknown/stale inputs. It copies policy into a
// stable-order receipt; no candidate is a durable deletion permit. Estimates
// are supplied logical bytes, not guaranteed reclaimable or additive disk space.
func Plan(now time.Time, input Input) (Receipt, error) {
	if now.IsZero() || validate(input) != nil {
		return Receipt{}, ErrInput
	}
	receipt := Receipt{Version: 1, Mode: "dry_run", Source: "supplied_metadata_only", CreatedAt: now.UTC(), Targets: make([]TargetReceipt, 0, len(input.Targets))}
	for _, t := range input.Targets {
		age, free := t.MinimumAgeHours, t.KeepFreeBytes
		if age == 0 {
			age = DefaultMinimumAgeHours
		}
		if free == 0 {
			free = DefaultKeepFreeBytes
		}
		row := TargetReceipt{ID: t.ID, Kind: t.Kind, Decision: "skip", KeepFreeBytes: free, MinimumAgeHours: age}
		row.Reason = skipReason(now, t, age, free)
		if t.Observation != nil {
			row.EstimatedBytes = t.Observation.EstimatedBytes
		}
		if row.Reason == "" {
			row.Decision, row.Reason = "candidate", "native_revalidation_required"
			if row.EstimatedBytes > math.MaxUint64-receipt.EstimatedCandidateBytes {
				return Receipt{}, ErrInput
			}
			receipt.EstimatedCandidateBytes += row.EstimatedBytes
		}
		receipt.Targets = append(receipt.Targets, row)
	}
	sort.Slice(receipt.Targets, func(i, j int) bool { return receipt.Targets[i].ID < receipt.Targets[j].ID })
	return receipt, nil
}

func skipReason(now time.Time, t Target, age int, keepFree uint64) string {
	if protected(t.Kind) {
		return "protected_target"
	}
	o := t.Observation
	if o == nil {
		return "observation_missing"
	}
	if o.ObservedAt.IsZero() || now.Sub(o.ObservedAt) > maxObservationAge {
		return "observation_stale"
	}
	if o.ObservedAt.After(now) {
		return "observation_future"
	}
	if o.AvailableBytes == nil {
		return "free_space_unknown"
	}
	if *o.AvailableBytes >= keepFree {
		return "free_space_sufficient"
	}
	if o.OpenHandles == Yes {
		return "open_handles"
	}
	if o.OpenHandles != No {
		return "open_handles_unknown"
	}
	if o.InUse == Yes {
		return "in_use"
	}
	if o.InUse != No {
		return "in_use_unknown"
	}
	if t.Kind == "worktree" {
		if o.WorktreeClean != Yes {
			return "worktree_not_proven_clean"
		}
		if o.FullyPushed != Yes {
			return "worktree_not_proven_pushed"
		}
	}
	if o.NewestEntryAt.IsZero() || o.NewestEntryAt.After(o.ObservedAt) {
		return "age_unknown"
	}
	if o.ObservedAt.Sub(o.NewestEntryAt) < time.Duration(age)*time.Hour {
		return "too_young"
	}
	return ""
}
