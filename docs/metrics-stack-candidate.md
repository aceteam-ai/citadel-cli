# Inactive S2 metrics app candidate

Refs aceteam-ai/aceteam#13204, #13201, #13200, #13209 and source S1
citadel-cli#1550. The existing S7 runbook is aceteam-ai/aceteam#13239.

`internal/apps/metricsstack.BuildCandidate` prepares JSON Compose source for a
single-node Prometheus and Grafana pair. Nothing imports it from a command,
worker or builtin catalog. It performs no IO, does not invoke an engine, and
returns rollout-authorized false with explicit required evidence. It changes no
generic image admission rule. A source merge cannot activate a metrics stack.

## Explicit operator inputs

There are no image or deployment-budget defaults. Each image must use a digest
in the existing approved AceTeam registry namespace. Syntax validation does not
prove who built the image, safety, architecture support, compatible flags or
non-root startup. The mirrored images and their provenance still need review;
fixtures use repeated synthetic digest characters, not selected artifacts.

Inputs include separate CPU, memory, PID and non-root UID/GID limits. The rendered
containers have read-only roots, dropped capabilities, no-new-privileges and
bounded scratch/log files. Automatic restart is disabled in this rehearsal
candidate. A native supervisor/restart policy needs its own reviewed wiring.
No host device, proc/sys mount, Docker socket, host networking or host port is
present. The sole network is internal. A stock exporter needing host visibility
must be installed through the explicitly approved isolated Phase A operator
path; it is not slipped into the generic app sandbox.

Both data volumes and both read-only configuration/provisioning volumes are
external and named under the instance namespace. They must be created and
permissioned by the operator before any use. No data volume is automatically
created, replaced, recursively chowned or deleted. There are no bind mounts or
credential values in the generated source. Re-rendering the same input preserves
volume and image identity. This is a restart-plan invariant, not a running
Prometheus persistence or restore acceptance result.

## Thirty-day sizing and real disk enforcement

The arithmetic matches S7: round up samples for 30 days, multiply active series
by measured bytes per sample, include head/WAL and other volume overhead, apply
the margin with upward rounding, and require the result within retention size.
Retention size plus other use and free reserve must fit the supplied disk budget.
The 50 percent minimum is enforced for stock Phase A only; native Phase B's
margin is independently reviewed. Integer overflow refuses the plan.

Other use must include Grafana, configuration and both bounded local container
log stores, not only TSDB blocks. The renderer reserves the configured logging
floor but cannot measure actual volume use. CPU/memory/query ceilings are source
policy for review, not performance measurements. Operator Phase A measurements
still set the real resource and disk caps and prove peak behavior.

Prometheus retention-size is not a filesystem quota. WAL, compaction and transient
blocks can exceed it. A dedicated bounded filesystem or independently verified
volume quota must enforce the storage allocation. `DiskBudgetBytes` is a
mathematical input, not proof that such a quota exists. The operator must retain
that evidence plus observed-peak and reserve receipts in S7 before installation.
An existing external volume does not establish a capacity or ownership proof.

## Configuration, ingress and identity gates

The renderer returns secret-free JSON/YAML-compatible baselines: Prometheus has
an empty scrape list, and an uneditable Grafana datasource refers only to the
internal Prometheus service. The operator supplies reviewed read-only overlay
configuration and provisioning; an empty scrape list proves no collection. No scrape target, new wire field, exporter token, shared admin key,
client secret, password or trusted-header identity is invented here. Prometheus
admin/lifecycle/remote-write receiver APIs are not enabled. Query concurrency,
time and sample limits are explicit. Prometheus is not a public query endpoint.

Grafana anonymous access, basic auth, initial admin creation, signup, public
sharing, external snapshots and unified alerting are disabled. Fleet incidents
remain the only pager. This is not complete admin authorization: login-form
hiding is not an authorization boundary, existing persisted accounts/settings
need audit, and S3 must prove current owning-org/admin admission and local-admin
route refusal before any native ingress. No ingress service or published port
exists in this candidate. The JSON must never be handed to a generic app install
path as proof of private-admin security. Phase A stays on an explicitly isolated
host/segment; it does not authorize a flat-mesh endpoint.

A reviewed fresh Grafana volume starts with no admin account. S3 supplies the
approved identity path later. Do not enable a default account as a workaround.
An internal Docker network also cannot reach arbitrary exporters: no egress
route or host-gateway mapping is invented. Phase A's isolated scrape adapter and
native S3 transport must be reviewed with their exact network configuration.

## Cold backup and disposable restore rehearsal

These are operator steps, not commands run by this lane. Use S7's phase packet
for signed-off measurements, case receipts and pass/failed/blocked outcomes.

1. Confirm the exact instance, both approved image digests, configuration digest,
   enforced volume capacities, non-root ownership and isolation receipts. Choose
   a fresh disposable restore namespace. Back up private configuration/identity
   material separately under the operator's approved encrypted custody; do not
   put secret values in a report or a GitHub artifact.
2. Stop both writers through the approved operator path and verify actual exit.
   A timeout or unknown stop is a failed cold-backup gate. Never copy a live
   Grafana SQLite database or claim a coherent TSDB copy from a live writer.
3. Back up both data volumes while stopped, retaining the complete Prometheus
   blocks, head and WAL. Record inventory, byte counts and content checksums.
   Save image/configuration references and the bounded volume identities. The
   Prometheus admin API remains off; do not enable it for this rehearsal.
4. Restore only into fresh operator-created quota-limited volumes. Preserve
   required ownership and permissions; refuse existing/foreign destinations.
   Verify content checksums and actual free/reserved capacity before starting.
5. Start the exact candidate in the disposable isolated environment. Prove
   samples and Grafana state survive restart and cold restore. Record restore
   duration, peak disk use and retention behavior, not only manifest equality.
   Check a deliberately failed scrape appears unknown/stale, never idle/healthy.
6. Verify every interface from an unauthorized isolated peer: no public or
   flat-mesh exporter, Prometheus UI or Grafana admin path. Prove authorized
   private-admin access only with the paired reviewed ingress. For stock Phase A
   retain its isolation proof; native Phase B also requires S3 and #11426.
7. Resume the source instance only after the operator confirms the rehearsal
   outcome and disposal scope. Do not delete original volumes or unrelated data.
   Retain failed/blocked evidence; synthetic unit tests cannot lift these gates.

No Docker, image download, exporter install, service operation, credential mint,
listener, enrollment, SQL, cluster, backup or restore action is performed by the
renderer or its fixture tests. Live persistence, image compatibility, private
admin access, backup/restore and measured sizing remain operator acceptance.

Primary configuration references:
[Prometheus storage](https://prometheus.io/docs/prometheus/latest/storage/) and
[Grafana settings](https://grafana.com/docs/grafana/latest/setup-grafana/configure-grafana/).
