# Typed container jobs

`containerjobs.Parse` is the node request boundary; the paired Aceteam
`utils.container_jobs` models own producer validation. The copied `wire-v1.json`
fixture pins the job family, canonical manifest bytes and its digest in both
repositories. Jobs contain typed argv and base64 bytes, never host shell strings.

`worker.ContainerJobHandler` advertises and executes the family only with the
machine's explicit owner-local `container_host` permission, a current enrolled
organization and complete local resource ceilings. `APPLY_DEVICE_CONFIG` may
revoke hosting but always refuses an enable request; omission preserves the
current grant. This permission
is separate from Shell and Files. The first adapter requires a privileged Linux
worker and installed Docker, ext4 and mount tools. No probe executes on handler
registration. A malformed permission file, unsupported platform/runtime or
missing permission directory refuses. Local `container_runtime` accepts the
Docker default (empty) or `runsc`; jobs cannot override it.

`container_egress_proxy_url` in the same node-local `permissions.yaml` is an
optional explicit HTTP forward proxy endpoint. It is empty by default
and is not an `APPLY_DEVICE_CONFIG` grant field or a job argument. For example,
the S5 host template can explicitly select:

```yaml
container_host: false
container_runtime: runsc
container_egress_proxy_url: http://host.docker.internal:3128
```

V1 pins the authority to `host.docker.internal:3128`, the S5 host-local proxy;
arbitrary hosts, numeric addresses, other ports and HTTPS proxy URLs refuse.
The S5 Squid template serves HTTP on that port, including CONNECT for HTTPS
destination traffic. Thus both HTTP_PROXY and HTTPS_PROXY use the HTTP endpoint;
this does not assume a TLS listener exists on the proxy itself.

When set, container create adds `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy` and
`https_proxy` with that exact node-configured URL; `NO_PROXY` and `no_proxy` are
fixed to `localhost,127.0.0.1,::1`. It also adds exactly
`host.docker.internal:host-gateway`. When unset, create adds neither proxy
environment variables nor a host mapping. Worker environment variables and
extra job fields do not select a proxy. Credentials, query/fragment tokens,
non-HTTP schemes, invalid ports and non-root URL paths refuse without reflecting
the URL. Changing the policy affects later creates; it does not mutate existing
containers or their network configuration.

This retains the inactive S3 proxy contract, not egress enforcement. Only an
empty network or explicit `none` is accepted, even with a proxy configured;
bridge, host and named-network requests refuse before native lookup. S5 egress
enforcement and a separately reviewed change must precede any connected network.
This source creates no bridge, starts no proxy and alters no firewall rules.

The native adapter pins Docker CLI and exec API traffic to the local Unix socket.
This is an explicit Docker-only exception to `catalog.ContainerRuntime`: that
interface does not supply loop-image provisioning or Engine exec completion
state. A closed program allowlist uses fixed system binary paths rather than
ambient PATH. Unknown programs refuse; worker credentials are not inherited.
`Engine.Run` acquires its process-safe host lock, rechecks permission and checks
container/volume ownership plus the container's actual attached volume. Lifecycle
operations retain the lock until actual return. The heavy execution lane also
retains the handler through cancellation/deadline drain. Failures are terminal
at the delivery boundary; automatic replay cannot make an uncertain create,
write, execution or purge safe.

`Native.CreateVolume` records the full requested quota in an exclusive sparse
image and syncs its reservation metadata before formatting or registration.
Capacity accounting subtracts every existing image's unallocated quota promise
from observed free space before admitting another reservation, preserving the
parent design's sparse-storage model without overcommitting those promises. Owner-local `container_max_quota_bytes`,
`container_max_volumes`, `container_max_memory_bytes`, `container_max_pids`,
`container_max_cpus` and `container_min_free_bytes` are all mandatory and
positive; none is a remotely writable grant. Missing or invalid limits withhold
the family. Counts include retained or partially created volume directories.
Free space is checked against outstanding promises, the new reservation and the local reserve under
the host lock. Create also checks CPU, memory and pids against those ceilings.
No default resource values activate a host.

Storage lives under the fixed root-only `/var/lib/citadel/container-volumes`,
independent of node/user configuration. FD-relative no-follow traversal checks
root ownership and non-writable ancestors before any directory creation; the
final host directory is private. Unsafe ancestry is refused, never repaired.
The lock is opened relative to that verified FD and must be a private regular
file owned by the worker with exactly one link. Subsequent privileged paths
remain below parents which a non-root principal cannot rename.

After reservation, provisioning formats ext4, mounts it with nodev/nosuid and registers a labeled
bind-backed Docker volume. Existing owned volumes are reused by container create
without formatting them, including imported or preserved disks. `OpenVolume`
verifies the expected filesystem and exact loop backing image before file access
or container start. An unmounted host directory is refused. Host boot/mount
restoration belongs to the operator host setup; this change installs no service.
After formatting and mounting a new volume, provisioning sets only its mounted
root to UID/GID `10001:10001`, the pinned S5 workspace image's `ace` identity,
before Docker volume registration. Mount or ownership failure refuses registration.
The private host image parent stays worker-owned. Existing volumes and imported
file trees are never recursively chowned or reformatted by this ownership step.
This root assignment does not resolve ownership of later privileged host-written
files or imported entries. That source follow-up is tracked in citadel-cli#1521;
full non-root workspace file/import acceptance stays held until it is resolved.
Container create applies CPU, memory, equal memory-swap, pids, dropped capabilities
and no-new-privileges. A bounded 4 GiB writable layer is mandatory: an unsupported storage driver or
rejected storage option refuses container creation. Network creation is absent;
cleanup may remove only a pre-existing network whose labels match the owning
container. Every new container uses network none.

`ContainerJobHandler` accepts only per-node deliveries and original payload
bytes; diagnostic maps cannot substitute rounded integers or another org.
`Engine.RunDelivery` compares request org to the current enrolled org before
locking and again after lock acquisition. Missing and foreign targets share
one terminal refusal. Eight-hex legacy and 32-hex runtime references are both
accepted; no new transport fields are introduced.

EXEC requires the original source job ID. Under the host flock it creates and
syncs a private hashed attempt record before executing. The record survives
restart and is never refunded after an uncertain write, sync, cancellation or
exec outcome. A repeated ID refuses rather than re-executing. This grants no
receipt recovery or new execution authority. The journal is bounded to 4096
records; full, malformed or uncertain storage refuses further EXEC. There is no
automatic pruning or operator reset path in this source slice. A separately
reviewed lifecycle is required before that bound can be recycled. The existing
serialized heavy lane is retained, including actual-exit custody; this adds
no admission slot or preemption authority.

EXEC uses Docker Engine create/start/inspect and bounded multiplexed stdout and
stderr. A newly created exec may have a null exit code; that state permits its
single start but never proves successful completion. An old unstarted exec
refuses, and completion requires a known exit code after observed start. Inspecting the actual exec state supplies the exit code. An attach loss
cannot prove exit or justify a second start; surviving execs from another worker
incarnation cannot inherit a new execution request. Output loss after verified
exit is reported as truncated/incomplete. On timeout, v1 stops the whole container
with SIGTERM and a ten-second grace, escalates to SIGKILL if necessary, and verifies
its immutable container ID is stopped. Only then does the result say:

> exec timed out; container stopped, files kept, it restarts on next use

The volume is retained. The caller's lifecycle layer performs the next-use start.
Unconfirmed stop returns an error. A future image-side supervisor can narrow this
to the timed-out process; v1 does not pretend killing an attach client does that.
See the [Docker Engine API](https://docs.docker.com/reference/api/engine/version/v1.51/).

File operations use a rooted filesystem handle and work while stopped. Reads and
writes are capped, file growth after metadata inspection is bounded, and special
files cannot block the worker's open. Export requires a stopped container and
walks directories in bounded batches. It preserves regular files, directories,
safe relative symlinks and hardlinks. Escaping/cyclic links and special files
refuse; nothing is silently omitted. Import writes only a newly created quota
volume, authenticates every chunk, checks canonical paths, and installs symlinks
after file extraction so they cannot redirect writes. Partial imports remain
owned, never successful or automatically purged. An explicit volume-only purge
is available after confirming no container is attached.

Transfer objects contain a fresh 12-byte nonce and AES-256-GCM ciphertext/tag.
Associated data binds protocol, org, transfer and chunk index. Manifest chunk
hashes cover complete encrypted objects; the fixture pins compact canonical JSON
for the whole manifest digest. HTTPS capability traffic refuses redirects,
userinfo, nonstandard ports, private/special/self addresses and mixed DNS answers,
and dials only validated numeric addresses. Archive entry/name/depth budgets and
chunk limits bound working memory. Transfer keys and URLs are never retained in
job diagnostics; the producer purges the queue payload after terminal handling.

This is source behavior, not native acceptance. Real quota exhaustion, stop/memory
release, runsc, reboot/mount recovery and node-to-node transfer proofs remain
operator acceptance before activation. Hosted placement's separate authorization,
network/firewall/proxy isolation and restricted handler set belong to S5/S6;
this source grants no customer access to an operator node. No live configuration,
credentials, SQL, service, deployment or release is changed by source tests.

The source tests use fake transports, fake command runners, temporary files and
small synthetic images only. Real Docker EXEC, privileged storage/mount behavior,
image/runtime containment, non-root file ownership (#1521), local owner setup
and the paired producer/intake contract remain explicit native/activation
acceptance gates. No live node is authorized by source tests.
