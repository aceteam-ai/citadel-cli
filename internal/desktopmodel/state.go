package desktopmodel

import (
	"encoding/json"
	"io"
)

type validationState struct {
	SchemaVersion       uint64 `json:"schema_version"`
	OperationID         string `json:"operation_id"`
	ManifestFingerprint string `json:"manifest_fingerprint"`
	Sequence            uint64 `json:"sequence"`
	Status              string `json:"status"`
	ErrorCode           string `json:"error_code"`
}

func (s validationState) valid() bool {
	if s.SchemaVersion != 1 || !lowerHex(s.OperationID, 32) || !lowerHex(s.ManifestFingerprint, 64) || s.Sequence == 0 {
		return false
	}
	switch s.Status {
	case "idle", "checking", "validated":
		return s.ErrorCode == ""
	case "rejected":
		return s.ErrorCode == "archive_invalid" || s.ErrorCode == "storage_unavailable"
	default:
		return false
	}
}

func newState(operation string, m manifest) (validationState, error) {
	fingerprint, err := m.fingerprint()
	if err != nil || !lowerHex(operation, 32) {
		return validationState{}, errState
	}
	return validationState{1, operation, fingerprint, 1, "idle", ""}, nil
}

func advanceState(s validationState, next string, m manifest, receipt *archiveReceipt, code string) (validationState, error) {
	fingerprint, err := m.fingerprint()
	if !s.valid() || err != nil || fingerprint != s.ManifestFingerprint || s.Sequence == ^uint64(0) {
		return validationState{}, errState
	}
	if s.Status == "idle" && next == "checking" && code == "" && receipt == nil {
		// Only validation work, not installation or runtime ownership.
	} else if s.Status == "checking" && next == "rejected" && receipt == nil && (code == "archive_invalid" || code == "storage_unavailable") {
	} else if s.Status == "checking" && next == "validated" && code == "" && receipt != nil && receipt.fingerprint == fingerprint && receipt.archiveSHA == m.ArchiveSHA256 && receipt.members > 0 && receipt.members <= memberLimit && receipt.payload > 0 && receipt.payload <= payloadLimit {
	} else {
		return validationState{}, errState
	}
	s.Sequence++
	s.Status, s.ErrorCode = next, code
	return s, nil
}

func (s validationState) canonical() ([]byte, error) {
	if !s.valid() {
		return nil, errState
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > stateLimit {
		return nil, errState
	}
	return b, nil
}

func parseState(r io.Reader) (validationState, error) {
	b, err := readBounded(r, stateLimit, errState)
	if err != nil {
		return validationState{}, err
	}
	v, err := flatObject(b, []string{"schema_version", "operation_id", "manifest_fingerprint", "sequence", "status", "error_code"}, stateLimit, errState)
	if err != nil {
		return validationState{}, err
	}
	var s validationState
	var ok bool
	if s.SchemaVersion, ok = unsigned(v["schema_version"]); !ok {
		return validationState{}, errState
	}
	if s.Sequence, ok = unsigned(v["sequence"]); !ok {
		return validationState{}, errState
	}
	if s.OperationID, ok = jsonString(v["operation_id"]); !ok {
		return validationState{}, errState
	}
	if s.ManifestFingerprint, ok = jsonString(v["manifest_fingerprint"]); !ok {
		return validationState{}, errState
	}
	if s.Status, ok = jsonString(v["status"]); !ok {
		return validationState{}, errState
	}
	if s.ErrorCode, ok = jsonString(v["error_code"]); !ok || !s.valid() {
		return validationState{}, errState
	}
	return s, nil
}

func lawfulPublication(previous, next validationState) bool {
	if !previous.valid() || !next.valid() || previous.Sequence == ^uint64(0) || next.Sequence != previous.Sequence+1 || previous.OperationID != next.OperationID || previous.ManifestFingerprint != next.ManifestFingerprint {
		return false
	}
	return (previous.Status == "idle" && next.Status == "checking") || (previous.Status == "checking" && (next.Status == "validated" || next.Status == "rejected"))
}
