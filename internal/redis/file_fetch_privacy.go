package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
	goredis "github.com/redis/go-redis/v9"
)

// Classify from the original stream entry, not a caller-supplied type. These
// credential-bearing deliveries are disposable on terminal ACK.
var acknowledgeSensitiveJob = goredis.NewScript(`
local entries = redis.call('XRANGE', KEYS[1], ARGV[2], ARGV[2])
local kind = ''
if #entries > 0 then
  local fields = entries[1][2]
  local payload = ''
  for i = 1, #fields, 2 do
    if fields[i] == 'type' then kind = fields[i + 1] end
    if fields[i] == 'payload' then payload = fields[i + 1] end
  end
  if kind == '' and payload ~= '' then
    local ok, decoded = pcall(cjson.decode, payload)
    if ok and type(decoded) == 'table' then kind = decoded['type'] end
  end
end
local acknowledged = redis.call('XACK', KEYS[1], ARGV[1], ARGV[2])
if acknowledged == 1 and (kind == 'FILE_FETCH' or kind == 'AMT_KVM') then redis.call('XDEL', KEYS[1], ARGV[2]) end
return acknowledged
`)

// AckSensitiveJob atomically acknowledges and removes a credential-bearing delivery.
func (c *Client) AckSensitiveJob(ctx context.Context, queue, messageID string) error {
	if queue == "" {
		queue = c.queueName
	}
	return acknowledgeSensitiveJob.Run(ctx, c.client, []string{queue}, c.consumerGroup, messageID).Err()
}

// AckFileFetchJob preserves the existing FILE_FETCH call site contract.
func (c *Client) AckFileFetchJob(ctx context.Context, queue, messageID string) error {
	return c.AckSensitiveJob(ctx, queue, messageID)
}

// MarshalJSON omits credential-bearing fields from retained job envelopes.
func (job Job) MarshalJSON() ([]byte, error) {
	type plain Job
	safe := plain(job)
	if jobprivacy.OmitPayload(job.Type) {
		safe.Payload, safe.RawPayload, safe.RawData = nil, nil, nil
	} else if jobprivacy.HasPasscode(job.Payload) {
		// The raw stream bytes carry the passcode too, so drop them.
		safe.Payload = jobprivacy.WithoutPasscode(job.Payload)
		safe.RawPayload, safe.RawData = nil, nil
	}
	return json.Marshal(safe)
}

func (job Job) Format(state fmt.State, verb rune) {
	if jobprivacy.OmitPayload(job.Type) {
		_, _ = io.WriteString(state, "<"+job.Type+" job; payload omitted>")
		return
	}
	type plain Job
	safe := plain(job)
	if jobprivacy.HasPasscode(job.Payload) {
		safe.Payload = jobprivacy.WithoutPasscode(job.Payload)
		safe.RawPayload, safe.RawData = nil, nil
	}
	_, _ = fmt.Fprintf(state, fmt.FormatString(state, verb), safe)
}
