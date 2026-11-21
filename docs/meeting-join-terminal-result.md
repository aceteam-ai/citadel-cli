# MEETING_JOIN retained terminal result

Issue: citadel-cli#1553. Consumer: aceteam#13175 (startup reconciliation of
non-terminal `meeting_sessions`).

## Why

A MEETING_JOIN publishes its terminal event once, on Pub/Sub channel
`stream:v1:<job_id>`, often hours after dispatch. If every coordinator replica
is restarting at that moment, nobody receives it and the transcript result is
lost. The node therefore also stores that terminal event in a Redis key the
coordinator can read when it starts.

## Contract

Retention is live in direct Redis mode and in API mode (the Redis API proxy,
the default node transport); see "API mode" below.

| Item | Value | Owner |
|------|-------|-------|
| Key | `job:<job_id>:result` | `worker.TerminalResultKey` |
| Value | The exact JSON envelope published on `stream:v1:<job_id>` for the job's first terminal event (`type` is `end`, `error` or `cancelled`) | `RedisStreamWriter.publishTerminal`, `APIStreamWriter.publishTerminal` |
| TTL | 72 hours | `worker.TerminalResultTTL` |
| Job types | `MEETING_JOIN` only | `worker.retainsTerminalResult` |

The value is a string holding one envelope, the same shape the backend already
parses from Pub/Sub. MEETING_JOIN runs through `LegacyHandlerAdapter`, so on an
`end` event `data.result` is `{"output": "<string>"}`, and that `output` string
is itself JSON (the handler's result object). The reader must decode twice:
once for the envelope, then again for `data.result.output`.

```json
{
  "version": "1.0",
  "type": "end",
  "jobId": "<job_id>",
  "rayId": "<ray_id>",
  "timestamp": "2026-11-09T12:00:00Z",
  "data": {
    "result": {"output": "{\"status\":\"completed\",\"meeting_id\":\"...\",\"transcript\":\"...\"}"},
    "meta": {"node_id": "...", "node_name": "..."}
  }
}
```

An `error` event carries `data.error` and `data.recoverable`; a `cancelled`
event carries `data.reason`. `data` carries whatever the published event
carried, including node metadata and delivery-clock fields when present.

## Write semantics

- **First writer wins.** `SET key value NX EX 259200`. A later terminal write
  for the same job id, whether a retried publish or the terminal refusal of a
  redelivered MEETING_JOIN, never replaces the stored result. When a publish
  is retried, the stored copy comes from the first attempt, so its `timestamp`
  (and any delivery-clock fields) can differ from the publish that succeeded.
- **Never blocks the publish.** The publish happens first. Retention runs after
  it whatever its outcome, with a 5-second bound and a context detached from
  job cancellation, so a shutdown that ends the meeting still leaves a result.
  A retention failure is logged and is never returned to the publish retry.

## Reading it

Read `job:<job_id>:result` for each non-terminal session whose job id is known,
parse the value as the Pub/Sub envelope, and apply it through the same
idempotent terminal path as a live event. A missing key means no terminal event
was produced, it expired, the node is an older version, or the envelope exceeded the API-mode size cap.

## API mode

API-mode nodes store the same envelope through the scoped proxy route, not the
generic `/api/fabric/redis/kv` route (which is not job-scoped, caps TTL at 24
hours and values at 1 MiB):

`POST /api/fabric/redis/jobs/{jobId}/result`, device key with
`device_redis:write`, body `{"envelope": "<terminal event JSON>", "ttl_seconds": 259200}`.
The server stores it with `SET NX EX` only for the job's assigned node in the
same org, accepts terminal events only, and answers `{"stored": true|false}`.

The node posts after the terminal publish, once, with the same 5-second bound
and detached context as direct mode. Any failure (4xx, 5xx, timeout) is logged
without credentials and never fails or retries the publish. The server caps an
envelope at 5 MiB; a larger one is skipped with a log line, never truncated
(`redisapi.MaxTerminalResultBytes`). The node-side owners are
`worker.apiTerminalRetainer` and `redisapi.Client.RetainTerminalResult`.
Durable HTTP deliveries (inactive, see
`docs/durable-http-delivery.md`) commit the terminal envelope through the
delivery itself and do not retain.
