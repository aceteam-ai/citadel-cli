# Optional heartbeat capacity signals

`status.collectCapacityMetrics` adds placement observations to the existing
`status.system` heartbeat object. Both the authenticated API and direct Redis
publishers serialize the same `status.SystemMetrics`; there is no new listener,
queue, scheduling decision or command surface.

| Fields | Meaning |
| --- | --- |
| `load1`, `load5`, `load15` | OS load averages, not percentages or divided by core count |
| `ncpu` | Logical CPUs available to the process at runtime startup (`runtime.NumCPU`), not physical cores or a dynamic CPU quota |
| `psi_cpu_some_avg60` | CPU pressure `some` over 60 seconds, kernel percent units |
| `psi_io_some_avg60`, `psi_io_full_avg60` | I/O pressure `some` and `full` over 60 seconds, kernel percent units |
| `psi_mem_some_avg60` | Memory pressure `some` over 60 seconds, kernel percent units |

Every field is optional. An observed zero is serialized as zero; a missing,
unsupported, failed or malformed observation is omitted. Never interpret an
omitted value as a healthy zero or as a reason to reject the heartbeat. Each
source is independent: missing PSI does not discard a valid load/core count,
and an invalid I/O `full` record does not discard a valid `some` record.

Linux's `nativeCapacityProbes` reads only `/proc/loadavg` and the fixed
`/proc/pressure/{cpu,io,memory}` sources. Each read is bounded before parsing by
`capacityReadLimit`, pinned in `TestCapacityReadBounds`. Kernels without PSI
simply omit pressure fields. Duplicate PSI records or `avg60` fields are
ambiguous and omitted; non-finite, negative or above-100 pressure values are
not published. Load values must be finite and nonnegative.

The non-Linux adapter uses the existing gopsutil load API where supported and
never reads Linux procfs. Unsupported load observation (including Windows)
stays absent, while a known logical CPU count can still be reported. PSI is
always absent off Linux. Cross-compilation does not prove native OS acceptance.

The existing system-metrics collector invokes these probes during status
collection. They do not enable lane inventory, enroll nodes, change credentials
or alter service/worker policy. No process names, hostnames or procfs record
suffixes are added to the wire; only the typed numeric observations above.

`TestCapacity*` uses fake readers, synthetic inputs and private fixture files.
The heartbeat transport proof uses in-process HTTP/Redis test doubles. These
tests do not call native capacity probes or sample the operator's machine.
