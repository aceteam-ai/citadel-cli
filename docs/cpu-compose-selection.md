# CPU compose selection

`platform.NvidiaGPUCountForCompose` owns a bounded observation for embedded
Ollama and llama.cpp compose selection. A positive count selects the GPU
variant. CPU selection requires no usable query result, a successful negative
hardware inventory and an absent NVIDIA control device. Missing hardware
inventory, inaccessible device evidence and timed-out probes are unknown.
Both commands share `ComposeGPUProbeTimeout`; no serialized start waits on an
unbounded driver query.

`services.SelectComposeForHost` preserves an existing flavor on unknown
evidence. A fresh file uses the embedded default until absence is proven.
`services.ComposeMapForRefresh` withholds uncertain variants from boot refresh,
so an upgrade cannot silently flip a GPU or CPU file after a failed probe.
The historical `.darwin.yml` filenames now contain shared CPU templates;
`TestDarwinComposeVariantsOnlyDropGPU` owns their allowed image/reservation
changes. CUDA-only engines remain outside this selection.

Initial configuration, CLI/module starts, worker starts and device-config
materialization use this selection. Device-config starts reconcile before the
runtime preflight. Changes are logged, but file replacement itself never
restarts a container; the existing start or refresh path owns runtime actions.

`compose.ReconcileCPUComposeOnDisk` replaces only a known embedded template's
GPU flavor, including the current node-directory namespace. Operator edits,
foreign namespaces, custom paths and same-flavor historical templates are
preserved. CLI callers use their service mutation lock; worker calls use the
serialized job lane. The byte recheck is not an interprocess lock.

`composerefresh.writeCompose` owns atomic publication and restrictive file mode.
It preserves an existing file's owner; a new file inherits the services
directory's owner. Failure to keep the owner refuses publication. Its pure
ownership decision follows the existing permissions-save policy for a group
change with the same owning user. The same writer publishes managed stamps.

`ReplaceManagedCompose` repairs an existing stamp's service hash without
changing its version or peers. A stamp error remains an error even if the
compose rename succeeded; a repeat can repair that hash. Atomic renames do not
promise crash durability across both updates. Existing reservation and
fine-tune admission guards remain authoritative.

`services.AvailableServicesForHost` filters the live heartbeat and Control
Center list using the same tri-state observation. On positively GPU-less Linux,
CUDA-only engines are withheld. `CUDAStartRefusal` owns the common reason used
by worker, CLI and device-config starts; boot skips each unsupported engine
with that reason and samples GPU evidence once. Positive or unknown evidence
retains the normal attempt, including driver failures on NVIDIA hardware.
The CPU-capable engines and existing Darwin policy are unchanged. Static CLI
help still describes the compiled registry; live scheduling uses host policy.
