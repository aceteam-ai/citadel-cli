package redis

import (
	"context"

	goredis "github.com/redis/go-redis/v9"
)

// A staged file arrives as base64 inside a stream entry (citadel-cli#1721).
// The worker knows from the job's own action that the entry is a stage_file,
// so this script does no decoding (the payload can be tens of megabytes): it
// acknowledges the delivery and, only if that ack took effect, deletes the
// entry so the file does not sit in the job stream.
var acknowledgeStagedFile = goredis.NewScript(`
local acknowledged = redis.call('XACK', KEYS[1], ARGV[1], ARGV[2])
if acknowledged == 1 then redis.call('XDEL', KEYS[1], ARGV[2]) end
return acknowledged
`)

// AckStagedFileJob acknowledges a stage_file delivery and removes its entry.
func (c *Client) AckStagedFileJob(ctx context.Context, queue, messageID string) error {
	if queue == "" {
		queue = c.queueName
	}
	return acknowledgeStagedFile.Run(ctx, c.client, []string{queue}, c.consumerGroup, messageID).Err()
}
