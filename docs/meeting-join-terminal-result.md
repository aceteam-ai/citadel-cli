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

| Item | Value | Owner |
|------|-------|-------|
| Key | `job:<job_id>:result` | `worker.TerminalResultKey` |
| Value | The exact JSON envelope published on `stream:v1:<job_id>` for the job's first terminal event (`type` is `end`, `error` or `cancelled`) | `RedisStreamWriter.publishTerminal`, `APIStreamWriter.publishTerminal` |
| TTL, direct Redis | 72 hours | `worker.TerminalResultTTL` |
| TTL, Redis API proxy | 24 hours (`TerminalResultTTL` clamped to the KV route maximum) | `redisapi.KVMaxTTL` |
| Size limit, Redis API proxy | 1 MiB; a larger envelope is not retained (the publish is unaffected) and the node logs it | `redisapi.KVMaxValueBytes` |
| Job types | `MEETING_JOIN` only | `worker.retainsTerminalResult` |

The value is a string holding one envelope, the same shape the backend already
parses from Pub/Sub:

```json
{
  "version": "1.0",
  "type": "end",
  "jobId": "<job_id>",
  "rayId": "<ray_id>",
  "timestamp": "2026-11-09T12:00:00Z",
  "data": {"result": {"status": "completed", "transcript": "..."}, "meta": {"node_id": "...", "node_name": "..."}}
}
```

`data` carries whatever the published event carried, including node metadata
and delivery-clock fields when present.

## Write semantics

- **First writer wins.** Direct Redis uses `SET key value NX EX 259200`. The API
  proxy KV route has no conditional set, so the node reads the key and writes
  only when it is absent. A later terminal write for the same job id, whether a
  retried publish or the terminal refusal of a redelivered MEETING_JOIN, never
  replaces the stored result.
- **Never blocks the publish.** The publish happens first. Retention runs after
  it whatever its outcome, with a 5-second bound and a context detached from
  job cancellation, so a shutdown that ends the meeting still leaves a result.
  A retention failure is logged and is never returned to the publish retry.
- **Durable HTTP deliveries** (inactive, see `docs/durable-http-delivery.md`)
  commit the terminal envelope through the delivery itself and do not retain.

## Reading it

Read `job:<job_id>:result` for each non-terminal session whose job id is known,
parse the value as the Pub/Sub envelope, and apply it through the same
idempotent terminal path as a live event. A missing key means no terminal event
was produced, it expired, or it exceeded the API proxy size limit.

## Known limits (API mode)

The API proxy KV route (`POST /api/fabric/redis/kv`) caps TTL at 24 hours and
values at 1 MiB. Its `job:` prefix is not scoped to the job's org or assigned
node for either read or write, so any device key with `device_redis:write`
that knows a job id can write that key, and the envelope's own fields
(including `data.meta`) are writer-supplied and prove nothing about origin.
Until the proxy binds the key to the job's assigned node, treat an API-mode
value as unauthenticated. The requested proxy change is recorded on
citadel-cli#1553.
