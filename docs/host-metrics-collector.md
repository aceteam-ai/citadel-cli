# Inactive fleet metrics collector

Refs aceteam-ai/aceteam#13203, #13201, #13200, #13205 and #13174.

`internal/hostmetrics` is a source candidate, with no active listener, command,
worker observer, catalog entry, heartbeat field or background goroutine. It
cannot be enabled by a release alone. S3 must supply reviewed scoped transport
and current node/org authorization after #11426. Enrollment and real host probes
remain operator work. Heartbeat remains the only online authority.

`Collector.Collect` serializes finite metric families into Prometheus text.
A missing/failed sensor has family-known zero and no fabricated numeric sample.
A family-known one means at least that family's explicitly supplied observation
is available, not that every sensor exists, the node is online, or a job is idle.
Partial families omit unavailable metrics. Source errors and hard budget,
identity, series, output, duplicate and timeout violations refuse the scrape.
Consumers must retain unknown/stale state when a scrape fails.

## Inputs and source semantics

`ProcSource` accepts an explicitly supplied read-only kernel filesystem; it never
constructs a real host filesystem in this slice. Fixed proc files cover aggregate
CPU ticks (excluding double-counted guest time), memory bytes, device read/write
bytes and network receive/transmit bytes. Temperature probes use a bounded set of
thermal-zone paths. The cpu0 core-throttle counter is one explicitly identified
sensor, not a claim about all cores, GPU throttling or platform thermal state.
Missing thermal zones and unsupported platforms remain unknown.

Process sampling is restricted to trusted worker/collector PID plus start-tick
pins. CPU ticks, start time and RSS pages come from the same stat record; process
names are discarded. A changed start time refuses attribution. Page size is an
explicit trusted adapter input. No process enumeration, cmdline, environment,
user home/config or arbitrary-path read exists. CPU ticks are raw kernel units,
not seconds: no assumed clock-tick frequency is hidden in the series. Diskstats
sector counts use the Linux ABI's 512-byte unit, including on 4K devices. Device
and partition counters remain separate; consumers must not sum overlapping
whole-device and partition series as distinct physical IO.

GPU observations enter through an optional typed callback; S1 does not run
nvidia-smi, NVML, Metal, device commands or a model. A shared GPU's host usage must
never be attributed to one job without a separately verified ownership mapping.
Native GPU adapter wiring and other OS adapters remain explicit acceptance work.

`JobTracker` bounds active jobs and reuses a fixed slot space. A private
`JobBinding` ties an exact job ID to slot plus generation; old or late callbacks
cannot mutate a replacement, including a retry using the same job ID. Optional
CPU/RSS usage stays absent until actually attributed by trusted lifecycle code.
The tracker never divides host totals among active jobs. Private mappings are
local correlation data, not an API response, public display, or Prometheus label.
Historical run correlation needs the existing private job receipts; this slice
does not add unbounded per-job history to the TSDB.

## Cardinality and read policy

Only validated numeric node IDs, finite family names and bounded ordinal slots
are labels. No disk/interface names, GPU UUIDs, process names/arguments, user
names, paths, IPs, tokens or job IDs become labels. Device slots are stable within
one collector process and never recycled to another device in that process.
Capacity exhaustion refuses collection. Job slot generations are numeric values,
not new labels, so completing many jobs does not grow time-series cardinality.
A collector restart can remap hardware slots; counter resets and this limitation
must be accounted for in the S3 identity/restart contract before activation.

Limits are mandatory constructor input. Source safety ceilings are 5 seconds,
4 MiB aggregate input, 64 KiB per file, 128 file-open attempts, 64 private slots
per family, 4096 output series and 512 KiB serialized output. These are refusal
ceilings, not measured deployment defaults. The fixture policy uses smaller
limits. No production CPU/memory/scrape budget is ratified by these fixtures;
Phase A measurements and review still set those budgets.

Collection is single-flight and uses context deadlines without spawning a new
reader goroutine on timeout. Read byte/open counts and output allocations are
bounded. A context cannot interrupt a kernel-stalled filesystem read; native
latency/cancellation behavior must be measured in Phase B before wiring a live
scrape path. Optional adapters must honor the same context and return bounded
observations. A live transport needs its own admission and concurrency policy.

## Verification and remaining gates

Synthetic proc/sys and lifecycle fixtures cover units, PID reuse, missing sensors,
privacy, caps, bounded reader consumption, concurrent refusal, deadlines,
uncertain data, job reuse and generation fences. No real host data is read.

Native acceptance and S3 transport are not proven. No dashboards, app deployment,
notification path, retention change, credential, listener or SQL are created.
S2 remains a separately reviewed package; S7's stock Phase A rehearsal remains
separate from the native Phase B acceptance.

Kernel field references: [proc](https://docs.kernel.org/filesystems/proc.html)
and [IO counters](https://docs.kernel.org/admin-guide/iostats.html).
