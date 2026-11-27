# Control-only worker role

`citadel work --role control` limits job consumption to the enrolled node's
own stream. Set `role: control` in the machine-convergent device `config.yaml`
to select the same policy for later worker starts, including the Control Center
worker. The command flag overrides that configured default for that process.
Do not replace the credential file with this single setting; preserve its other
fields and owner-only permissions. An omitted role or `full` preserves legacy
queue selection. Unknown values refuse startup.

The policy is a startup snapshot. Editing config does not change a running
source. Restart must be an independently authorized operator action. Startup
logs name the effective role and this restart requirement. `citadel status`
labels configured and active roles separately. Its JSON `role` is the observed
active non-default role, and `configured_role` is the non-default saved policy.
The cheap `/worker` response, live `/status` and heartbeat `status.role` report
the running process's role, including a command-line override. Full/legacy
payloads omit this additive field. An unavailable active observation is unknown,
not inferred from config. A configured value can differ from a running process
until restart.

`resolveWorkerQueues` and `resolveDirectRedisQueuesForRole` own queue policy.
Control skips the platform-assigned queue, CPU, organization shell, inference,
tag and meeting pools even with an explicit queue or detected GPU. It requires
an enrolled organization and resolved node ID before source construction and
refuses startup if either is missing. It never uses a source's shared-queue
default. Runtime additions, including resubscribe and inference reconciliation,
cannot broaden a pinned source. Identity churn during startup also refuses.

Per-node handlers and their existing permission checks are unchanged. Config,
MODULE_SET and updates still use the per-node stream; future typed session and
inventory jobs can do the same. Role is scheduling policy, not a new permission,
credential or isolation boundary. It does not disable explicitly configured
services or permit jobs that the existing handler policy denies.

The separate HPC profile retains its job-local enrollment and per-node-only
queue contract. Its existing positive flag list refuses the ordinary `role`
flag rather than ignoring it.

Source tests use private paths, synthetic GPU facts and a fake Redis broker.
The broker proof delivers a config job through the real source and legacy
adapter to a fixture-only applier, leaving shared jobs untouched. It does not
run the native config handler, services or a live worker. Native acceptance and
changing a production node requires separate operator authorization after review.
