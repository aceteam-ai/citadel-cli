# Workflow LLM destination policy

Workflow `LLM` nodes use Ollama's `/api/chat` request/response schema. A graph
selects an endpoint, but cannot grant itself additional network authority.
`workflow.LLMPolicy.resolveURL` owns destination validation; the general workflow
HTTP node has its own independent policy.

## Defaults and exact grants

Without a policy file, the LLM node permits public HTTP(S) endpoints and the
exact built-in endpoint `http://127.0.0.1:11434/api/chat`. An omitted `endpoint`
uses that built-in URL. This is not a blanket loopback exception: `localhost`,
another IP spelling, another host, another scheme, another port or another path
needs an explicit grant when it reaches a loopback, LAN or mesh address. Public
endpoints retain the ordinary public-destination policy, not a grant requirement.

For other private model services, the node operator can create
`workflow-llm-policy.yaml` in the machine-local node configuration directory.
`network.GetNodeConfigDir()` owns the directory choice, the same directory used
for `workflow-http-policy.yaml`; do not substitute the workspace directory or
the invocation's user-local CLI configuration directory.

For example, these are two separate, exact private-service grants:

```yaml
allow:
  - endpoint: http://192.168.1.2:11434/api/chat
  - endpoint: http://100.64.0.58:11434/api/chat
```

Use IP literals where possible. A hostname grant trusts that name's DNS for
loopback/private answers, although hard-denied addresses remain blocked. In
particular, `localhost` and `127.0.0.1` are separate endpoint identities. A
`localhost` grant may select `::1`; it will fail if the service only listens on
IPv4. The client validates every DNS answer, pins the first validated numeric
address, and does not try the other answers as a connection fallback.

The file must be a regular file, not a symlink, at most 16 KiB, with at most 64
entries in the closed schema above. Unknown or duplicate keys, duplicate
canonical endpoints, aliases, extra YAML documents and malformed endpoints are
invalid. `allow: []` adds no grants; it does not remove the built-in endpoint.
To keep the defaults, leave the policy absent or use `allow: []`; do not create
an empty file. An empty or whitespace-only file is an invalid policy and
disables all LLM destinations, including the built-in endpoint.
Keep the node configuration directory under the node operator's control. Do not
set the job workspace to that directory: workspace file-write authority must not
become policy-write authority.

## Loading and updates

`workflow.LoadLLMPolicy` reads the file once at executor construction. Both worker
entrypoints load from the node directory when the worker starts. After creating,
editing or removing the policy, restart the worker (`citadel work` service or the
Control Center-managed worker) through your normal operational procedure. There
is no hot reload; changing the file does not change an already-running executor.

A missing file or missing node directory retains the defaults. An empty directory
argument, unreadable file, malformed document, oversized file, symlink or file
replacement during loading disables **all** LLM destinations, including the
built-in Ollama endpoint. It does not fall back to an unrestricted client. The
worker emits a static warning with a bounded `reason`, such as
`invalid_policy_document`, `file_too_large`, `file_not_regular`, or
`open_permission_denied`. It never logs the policy text, path or raw filesystem
error. Correct the policy and restart the worker to load it again.

A valid file containing an IP-literal grant for an always-denied destination
produces one static warning per load with reason `ineffective_literal_ip_grant`.
The grant remains ineffective; the document is not rejected and its other grants
and defaults are unchanged. Loading does not resolve hostname grants, whose
answers are checked only at request time. The warning includes no endpoint or
policy content.

## Endpoint and transport boundaries

Explicit grants match the canonical full URL: scheme, canonical host, effective
port and exact escaped path. Host case and a trailing DNS dot normalize; an
omitted standard port equals `:80` for HTTP or `:443` for HTTPS; an empty path
equals `/`. Paths otherwise remain distinct: `/api/chat/`, `/api/%63hat`,
`/api/delete`, `/api/pull` and `/api/x/../chat` are not `/api/chat` grants.
Canonical address spellings may match an explicit IP grant, but the built-in
exception only recognizes its exact URL spelling.

Grants can allow only loopback or private addresses, including the private mesh
range. They cannot allow metadata/link-local, unspecified, multicast, broadcast,
node-local DNS/service, reserved or translated IPv6 addresses. Every DNS answer
must pass these checks before a connection is made. There are no wildcard or
CIDR-wide LLM grants, graph/payload grants, HTTP-policy inheritance or unrestricted
"allow LAN" switch.

Userinfo, queries, fragments, scoped addresses, opaque/non-HTTP(S) URLs, malformed
hosts/ports and whitespace/control characters are refused. Connections use a
validated numeric address without a second lookup, no proxy or redirect, and
ordinary TLS certificate and hostname verification. The wire `Host` omits standard
`:80`/`:443` ports without changing the canonical grant identity; nonstandard
ports are retained.

A single five-minute request context covers DNS, dial, TLS, headers and body,
subject to the caller's earlier deadline or cancellation. Responses larger than
10 MiB are refused, not truncated into success. Failures expose static classes
and numeric HTTP status codes, never raw network errors, URLs, response bodies,
prompts or credentials.

The built-in endpoint and grants do not install, start or trust a model service,
grant workflow caller authorization or Shell permission, or reserve inference
capacity. An authorized workflow can request an installed local model through
the built-in endpoint, which can contend for VRAM. Client cancellation does not
prove the model server stopped inference, evict a service or establish GPU/process
exit custody.
