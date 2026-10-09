# Versioned binary range reads

`FILE_READ_BYTES_RANGE_V1` is the bounded range contract for node files whose total size can exceed 50 MiB. It is a separate job type from `FILE_READ_BYTES`, so an older node cannot mistake a large-file range request for a whole-file read. The node advertises it in `capabilities.job_types` only while the Files permission and workspace are available.

The payload uses string values: `path`, `offset` (zero-based), and `length` (positive, at most 8 MiB). Optional `max_bytes` is a decoded **response** ceiling, not a total-file ceiling; it must be positive, at most 8 MiB, and at least `length`. The handler reads only the requested range and never loads or encodes the whole file. Offsets beyond the end and integer overflow are rejected. An offset exactly at EOF returns an empty chunk.

The JSON result contains `contract_version: 1`, `encoding: "base64"`, `content`, `offset`, `size` (decoded bytes), `total_size` (stat of the opened file), and `eof`. The returned size is at most the requested length and 8 MiB; the final range is clamped to EOF. A short read caused by a concurrent file change fails rather than returning contradictory metadata. Callers must validate every field and keep their own response-size bound before base64 decoding.

`FILE_READ_BYTES` keeps its 50 MiB whole-file ceiling, including when a larger `max_bytes` is supplied. Its existing range form also refuses files over 50 MiB. For mixed versions, a coordinator must check the target node's live `capabilities.job_types` for `FILE_READ_BYTES_RANGE_V1` before dispatching a large-file range. Missing or stale capability fails closed; never raise the legacy `max_bytes` ceiling as a fallback. For files within 50 MiB, the legacy contract remains available.

AceTeam #10824 and draft #10833 need a separate follow-up to select the new job type only after that positive capability check, validate `contract_version` and range metadata, retain the 1 MiB per-response limit in their relay, and leave older nodes with a clear unsupported state. This Citadel change does not roll out or restart nodes.
