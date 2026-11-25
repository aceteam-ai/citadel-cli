# local-flow-run node template builtin

Issue: citadel-cli#1543. Parent: aceteam#13129. Engine: the Python
`local_flow` package from aceteam#13130.

`local-flow-run` is a compiled-in `RUN_JOB_TEMPLATE` builtin. It runs one
pinned Flow package in the node's isolated Python engine container and returns
a bounded result with a signed receipt. It is inactive in this release: no
engine image is pinned, so every run is refused as "engine unavailable".

## Owners

- `cmd.runLocalFlowBuiltin` and `cmd.admitLocalFlowRun` own admission and
  execution (`cmd/template_builtin_local_flow.go`).
- `cmd.liveTemplateRunOps.Run` owns the attested path: it preflights the
  signer, passes the node-trusted `templateRunBinding`, and signs the receipt
  with `signAttestedReceipt` before the attempt is marked complete.
- `worker.templateQueueOrg` derives the organization from the exact per-node
  stream `jobs:v1:shell:org_<id>:node:<node>`. Params never supply identity.
- `localFlowInputSchema`, `localFlowOutputSchema` and `localFlowRunner` are the
  only manifest this handler runs. `TestLocalFlowPinnedManifestHash` pins its
  content hash against Python's `compute_template_hash`. The platform must
  register exactly this manifest.

## Engine contract (aceteam#13130, draft head 3e82a089)

The image entrypoint is `python -m local_flow`, a one-request CLI. There is no
HTTP endpoint and no health check.

- stdin: `{"graph":{...},"input":{...},"package_digest":"sha256:...","run_id":"<uuid>"}`,
  at most 1400 KiB. The node sends the canonical bytes it hashed.
- stdout, success: `{"output":{...},"usage":{"input_tokens":n|null,"output_tokens":n|null}}`.
- stdout, refusal (exit 1): `{"error":"<fixed category>"}`. Unknown categories
  are reported as `local execution failed`; engine text is never echoed.
- env: `CITADEL_LOCAL_RUNS_DIR=/runs`, `CITADEL_LOCAL_GATEWAY_PORT`,
  `CITADEL_LOCAL_MODELS` (JSON list). All three are always set.

`package_digest` is the sha256 of the graph's Python-compatible canonical JSON
(`json.dumps(sort_keys=True, separators=(",", ":"))`). It is separate from the
engine image digest. `TestLocalFlowPackageDigestMatchesPython` pins both a
plain and a non-ASCII fixture against Python.

## Request

Params are closed and exact-key (both the schema gate and
`decodeBuiltinParams`): `node_id`, `organization_id`, `flow_version_id`,
`run_id`, `package_digest`, `graph_json`, `input_json`. The graph and input
travel as JSON strings because the closed-schema gate rejects free-form
objects; they are decoded with `jobs.DecodeTemplateJSON`, which refuses
duplicate keys.

## Refusals, all before the container starts

1. A manifest other than the pinned one, even with a self-consistent hash.
2. Any input file, or a missing node, job or organization identity.
3. Unknown or alternate-case params, or a malformed value.
4. `node_id` other than this node, or `organization_id` other than the stream's.
5. A graph whose canonical digest differs from `package_digest`, oversized
   graph or input, wrong top-level keys, or a node type outside the engine's
   allowlist.
6. Any `LocalInference` (LLM) node, with the fixed reason
   `llm_unavailable_without_base_url` (see Network below).
7. No usable receipt signer (`jobs.ErrReceiptSigningUnavailable`).
8. No pinned image, or the pinned image not present locally. The node never
   pulls (`--pull never`).
9. cpu, memory or pids limits that rootless Podman cannot enforce.
10. A run id this node already ran (ledger below).

There is no fallback. A refused run never runs on hosted infrastructure or on
another node.

## Container

`run --rm -i --pull never --cap-drop ALL --security-opt no-new-privileges
--read-only --user 65532:65532 --cpus 1 --memory 1g --pids-limit 256`, tmpfs
`/runs` (64 MiB) and `/tmp` (16 MiB) through `--mount`, only the three env
values, and the digest-pinned image last with nothing after it. Commands go
through the `catalog.ContainerRuntime` seam. stdout is capped; a killed or
timed-out run removes its named container.

## Network

Every run uses `--network none`; `buildLocalFlowRunArgs` refuses any other
mode, and host networking is never used. The engine's inference client
currently dials a hardcoded `http://127.0.0.1:<port>`, which only host
networking could reach from inside the container. So a graph with any
`LocalInference` node is refused before launch with
`llm_unavailable_without_base_url`, and only deterministic graphs run.

Planned path for LLM graphs (follow-ups, not built here):

1. aceteam#13130's engine reads its model endpoint from an environment
   variable instead of hardcoding 127.0.0.1.
2. Citadel runs the container on a per-run bridge network and starts a per-run
   forwarder bound to that network's gateway address. The forwarder passes
   only `POST /v1/chat/completions` for the run's allowed model to the local
   engine, which stays loopback-only. The engine endpoint variable points at
   the forwarder; nothing else on the host is reachable.

## Run-id ledger

`<node config dir>/local-flow-runs/<run_id>`, created with `O_EXCL` as the last
step before launch, so a refused run does not consume its id and two concurrent
deliveries cannot both start. Entries older than 30 days are pruned. The engine's
own duplicate check cannot work here because `/runs` is fresh per run.

## Result and receipt

- `outputs`: one `result.json` holding the canonical `{output, usage}`.
- `report`: the inline output and usage, the `binding` object and its hash.
- `usage`: ledger attribution (`origin: dispatched`, `org_id`,
  `action: run_job_template`, `manifest_sha256`, `signed: true`). Token counts
  the engine could not measure stay null and `tokens_known` is false; the node
  usage database still stores them as 0.
- `aep_receipt` (also `receipt` and `work_receipts`): a v2 `run_job_template`
  receipt, which the platform verifier already treats as event-style.
  `input_sha256` is the hash of `binding` in Python-compatible canonical JSON:
  template key, version and content hash, node, organization, job, run id,
  Flow version, package digest, input digest, engine image and network mode. `output_sha256` is the hash of the ordered output digests, as for
  other templates. Signing is not gated on an environment flag and never fails
  open; a signing failure removes the attempt's output.

## Activation gates (not met in this change)

- LLM graphs: the engine endpoint variable and the per-run bridge forwarder
  above.
- A release publishes the aceteam#13130 image and sets `localFlowPinnedImage`
  to `repository@sha256:...`.
- aceteam#13130 merges with the contract above, or this builtin follows it.
- The platform registers the pinned manifest, dispatches `node_id` and
  `organization_id`, recomputes `binding_sha256`, and stores usage and receipts.
- Shipped-artifact acceptance on a disposable owned node, with the image
  already pulled:

  ```
  CITADEL_LOCAL_FLOW_ARTIFACT_IMAGE=<repository>@sha256:<digest> \
    go test ./cmd -count=1 -run TestLocalFlowShippedArtifact
  ```

  The test is skipped without that variable and never pulls an image.
