# Typed container jobs

`containerjobs.Parse` is the node request boundary; the paired Aceteam
`utils.container_jobs` models own producer validation. The copied `wire-v1.json`
fixture pins the job family, canonical manifest bytes and its digest in both
repositories. Jobs contain typed argv and base64 bytes, never host shell strings.

`worker.ContainerJobHandler` advertises and executes the family only with the
machine's explicit `container_host` permission. `APPLY_DEVICE_CONFIG` accepts
`containerHostEnabled`; omission preserves the current grant. This permission
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
container_host: true
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

This supplies the S3 client contract, not egress enforcement. A named bridge is
still required for connectivity; an absent network remains `none` even with a
proxy configured. S5 must bind the proxy to the mapped host endpoint and permit
only that destination/port through its default-deny firewall. The proxy owns
domain allowlisting; `NO_PROXY` cannot bypass the firewall. This source does not
start a proxy or alter firewall rules.

The native adapter pins Docker CLI and exec API traffic to the local Unix socket.
`Engine.Run` acquires its process-safe host lock, rechecks permission and checks
container/volume ownership plus the container's actual attached volume. Lifecycle
operations retain the lock until actual return. The heavy execution lane also
retains the handler through cancellation/deadline drain. Failures are terminal
at the delivery boundary; automatic replay cannot make an uncertain create,
write, execution or purge safe.

`Native.CreateVolume` creates an exclusive private image with exactly the
requested quota, formats ext4, mounts it with nodev/nosuid and registers a labeled
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
and no-new-privileges. Writable layer sizing is requested where the storage driver
supports it; an unsupported option fails rather than weakening the request.
Named bridges are confined by org/container labels and are removed with their
owning container. An absent network selects none.

EXEC uses Docker Engine create/start/inspect and bounded multiplexed stdout and
stderr. Inspecting the actual exec state supplies the exit code. An attach loss
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
