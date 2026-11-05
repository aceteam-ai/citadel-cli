package status

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const WorkerDetailMaxBytes = 256 << 10

const (
	workerDetailStringBytes  = 256
	workerDetailRecentLimit  = 64
	workerDetailEvictedLimit = 64
	workerDetailLaneLimit    = 16
)

var ErrWorkerDetailUnavailable = errors.New("worker detail unavailable")

// WorkerDetail is an opt-in read-only projection on the existing local control
// surface. Nil subsystem pointers/slices mean unknown, not healthy zeroes.
// ObservedAt marks this read; the providers are not an atomic cross-subsystem
// snapshot. PID and uptime retain the existing session identity limitations.
type WorkerDetail struct {
	Mode       string          `json:"mode"`
	Connected  bool            `json:"connected"`
	NodeName   string          `json:"node_name,omitempty"`
	MeshIP     string          `json:"mesh_ip,omitempty"`
	Version    string          `json:"version"`
	PID        int             `json:"pid"`
	UptimeSec  int64           `json:"uptime_seconds"`
	ObservedAt time.Time       `json:"observed_at"`
	Worker     *WorkerLiveness `json:"worker,omitempty"`
	Swap       *SwapActivity   `json:"swap,omitempty"`
	Lanes      []LaneActivity  `json:"lanes"`
}

// ValidateWorkerDetail checks finite collection and byte limits before any
// cloning or encoding. It never truncates an observation to make it fit.
func ValidateWorkerDetail(detail WorkerDetail) error {
	validString := func(value string) bool {
		return len(value) <= workerDetailStringBytes && utf8.ValidString(value)
	}
	if (detail.Mode != "worker" && detail.Mode != "presence") || detail.PID <= 0 || uint64(detail.PID) > 1<<32-1 || detail.UptimeSec < 0 || detail.ObservedAt.IsZero() || detail.Version == "" {
		return ErrWorkerDetailUnavailable
	}
	for _, value := range []string{detail.Mode, detail.NodeName, detail.MeshIP, detail.Version} {
		if !validString(value) {
			return ErrWorkerDetailUnavailable
		}
	}
	if live := detail.Worker; live != nil {
		if !validString(live.PubSubTransport) || live.InFlight < 0 || live.Processed < 0 || live.Failed < 0 {
			return ErrWorkerDetailUnavailable
		}
	}
	if swap := detail.Swap; swap != nil {
		if len(swap.Recent) > workerDetailRecentLimit || swap.SwapsPerHour < 0 || swap.EvictingSwapsPerHour < 0 || swap.MaxEvictingPerHour < 0 {
			return ErrWorkerDetailUnavailable
		}
		for _, record := range swap.Recent {
			if len(record.Evicted) > workerDetailEvictedLimit || !validString(record.Backend) || !validString(record.Model) || !validString(record.Outcome) || record.Wait < 0 {
				return ErrWorkerDetailUnavailable
			}
			for _, evicted := range record.Evicted {
				if !validString(evicted) {
					return ErrWorkerDetailUnavailable
				}
			}
		}
	}
	if len(detail.Lanes) > workerDetailLaneLimit {
		return ErrWorkerDetailUnavailable
	}
	for _, lane := range detail.Lanes {
		if !validString(lane.Lane) || lane.Lane == "" || lane.Queued < 0 || lane.Executing < 0 || lane.ExecCapacity < 0 {
			return ErrWorkerDetailUnavailable
		}
	}
	return nil
}

func CloneWorkerDetail(detail WorkerDetail) (WorkerDetail, error) {
	if err := ValidateWorkerDetail(detail); err != nil {
		return WorkerDetail{}, err
	}
	cloneTime := func(value *time.Time) *time.Time {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	if detail.Worker != nil {
		live := *detail.Worker
		live.LastJobConsumedAt = cloneTime(live.LastJobConsumedAt)
		live.LastPollAt = cloneTime(live.LastPollAt)
		detail.Worker = &live
	}
	if detail.Swap != nil {
		swap := *detail.Swap
		if swap.Recent != nil {
			swap.Recent = append([]SwapRecord{}, swap.Recent...)
			for i := range swap.Recent {
				record := &swap.Recent[i]
				if record.Evicted != nil {
					record.Evicted = append([]string{}, record.Evicted...)
				}
				if record.Pulled != nil {
					pulled := *record.Pulled
					record.Pulled = &pulled
				}
			}
		}
		detail.Swap = &swap
	}
	if detail.Lanes != nil {
		detail.Lanes = append([]LaneActivity{}, detail.Lanes...)
		for i := range detail.Lanes {
			detail.Lanes[i].BusySince = cloneTime(detail.Lanes[i].BusySince)
		}
	}
	return detail, nil
}

func EncodeWorkerDetail(detail WorkerDetail) ([]byte, error) {
	owned, err := CloneWorkerDetail(detail)
	if err != nil {
		return nil, err
	}
	// Object fanout and string sizes are already bounded before Marshal can
	// allocate. Check the complete serialized reply, including JSON escaping.
	raw, err := json.Marshal(owned)
	if err != nil || len(raw) > WorkerDetailMaxBytes {
		return nil, ErrWorkerDetailUnavailable
	}
	return raw, nil
}

// DecodeWorkerDetail accepts additive fields but refuses duplicate names at
// every depth, case aliases of known members, null required values, malformed
// subsystem objects, oversized strings/collections and trailing JSON values.
func DecodeWorkerDetail(raw []byte) (WorkerDetail, error) {
	if len(raw) > WorkerDetailMaxBytes || !utf8.Valid(raw) {
		return WorkerDetail{}, ErrWorkerDetailUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := workerDetailJSONValue(decoder, 0); err != nil {
		return WorkerDetail{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return WorkerDetail{}, ErrWorkerDetailUnavailable
	}
	fields, err := workerDetailObject(raw, []string{"mode", "connected", "version", "pid", "uptime_seconds", "observed_at"}, []string{"node_name", "mesh_ip", "worker", "swap", "lanes"})
	if err != nil {
		return WorkerDetail{}, err
	}
	if live := fields["worker"]; workerDetailPresent(live) {
		if _, err := workerDetailObject(live, []string{"consuming", "in_flight"}, []string{"last_job_consumed_at", "last_poll_at", "processed", "failed", "identity_unresolved", "pubsub_transport"}); err != nil {
			return WorkerDetail{}, err
		}
	}
	if swap := fields["swap"]; workerDetailPresent(swap) {
		swapFields, err := workerDetailObject(swap, []string{"swaps_per_hour", "evicting_swaps_per_hour", "max_evicting_per_hour"}, []string{"recent"})
		if err != nil {
			return WorkerDetail{}, err
		}
		var records []json.RawMessage
		if json.Unmarshal(swapFields["recent"], &records) != nil && len(swapFields["recent"]) != 0 {
			return WorkerDetail{}, ErrWorkerDetailUnavailable
		}
		if len(records) > workerDetailRecentLimit {
			return WorkerDetail{}, ErrWorkerDetailUnavailable
		}
		for _, record := range records {
			recordFields, err := workerDetailObject(record, []string{"backend", "started_at", "wait", "outcome"}, []string{"model", "evicted", "pulled"})
			if err != nil {
				return WorkerDetail{}, err
			}
			if evicted := recordFields["evicted"]; workerDetailPresent(evicted) {
				var names []json.RawMessage
				if json.Unmarshal(evicted, &names) != nil || len(names) > workerDetailEvictedLimit {
					return WorkerDetail{}, ErrWorkerDetailUnavailable
				}
				for _, name := range names {
					var text string
					if !workerDetailPresent(name) || json.Unmarshal(name, &text) != nil {
						return WorkerDetail{}, ErrWorkerDetailUnavailable
					}
				}
			}
		}
	}
	if lanes := fields["lanes"]; workerDetailPresent(lanes) {
		var records []json.RawMessage
		if json.Unmarshal(lanes, &records) != nil || len(records) > workerDetailLaneLimit {
			return WorkerDetail{}, ErrWorkerDetailUnavailable
		}
		for _, lane := range records {
			if _, err := workerDetailObject(lane, []string{"lane", "queued", "executing", "exec_capacity"}, []string{"busy_since"}); err != nil {
				return WorkerDetail{}, err
			}
		}
	}
	var detail WorkerDetail
	if json.Unmarshal(raw, &detail) != nil {
		return WorkerDetail{}, ErrWorkerDetailUnavailable
	}
	return CloneWorkerDetail(detail)
}

func workerDetailPresent(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func workerDetailObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrWorkerDetailUnavailable
	}
	known := append(append([]string{}, required...), optional...)
	for key := range fields {
		for _, exact := range known {
			if key != exact && strings.EqualFold(key, exact) {
				return nil, ErrWorkerDetailUnavailable
			}
		}
	}
	for _, key := range required {
		if !workerDetailPresent(fields[key]) {
			return nil, ErrWorkerDetailUnavailable
		}
	}
	for _, key := range optional {
		if raw, exists := fields[key]; exists && !workerDetailPresent(raw) {
			switch key {
			case "worker", "swap", "lanes", "last_job_consumed_at", "last_poll_at", "busy_since", "pulled", "recent", "evicted":
			default:
				return nil, ErrWorkerDetailUnavailable
			}
		}
	}
	return fields, nil
}

func workerDetailJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrWorkerDetailUnavailable
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrWorkerDetailUnavailable
	}
	switch value := token.(type) {
	case string:
		if len(value) > workerDetailStringBytes {
			return ErrWorkerDetailUnavailable
		}
	case json.Delim:
		switch value {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || len(name) > workerDetailStringBytes || seen[name] {
					return ErrWorkerDetailUnavailable
				}
				seen[name] = true
				if err := workerDetailJSONValue(decoder, depth+1); err != nil {
					return err
				}
			}
			if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
				return ErrWorkerDetailUnavailable
			}
		case '[':
			for decoder.More() {
				if err := workerDetailJSONValue(decoder, depth+1); err != nil {
					return err
				}
			}
			if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
				return ErrWorkerDetailUnavailable
			}
		default:
			return ErrWorkerDetailUnavailable
		}
	}
	return nil
}
