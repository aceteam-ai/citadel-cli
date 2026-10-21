# Workflow LLM destination policy (#1316)

Source-only DRAFT design and implementation plan. #1358 repaired the HTTP node;
#1316 also requires the LLM node to stop using an unrestricted HTTP client.
This draft composes the shared address/transport hardening from DRAFT #1401
with current main, preserving both histories. That dependency still needs its
own independent review. No deployment, live policy write, local candidate
execution, undraft or merge is authorized by this document.

## Proposed security and compatibility contract

- An LLM graph chooses a URL, not network authority. The default policy permits
  only public HTTP(S) destinations. There is no implicit localhost exception.
- Local Ollama remains an explicitly configurable target, not a blanket bypass.
  The machine-local `workflow-llm-policy.yaml` can grant an exact full endpoint,
  including scheme, canonical host, effective port and exact escaped path.
  Neither a graph, its inputs nor the job payload can supply policy grants.
- An endpoint grant can permit loopback or private addresses for that endpoint
  only. It cannot permit link-local/metadata, unspecified, multicast, broadcast,
  node-local DNS/service, reserved or translated IPv6 ranges. No wildcard,
  CIDR-wide exception, HTTP-policy inheritance or "allow LAN" switch is added.
- Validate every DNS result, then pin a numeric connection to one validated
  address. Disable proxies and redirects. Retain ordinary TLS certificate and
  hostname verification. Reuse the existing transport, not a second DNS lookup
  or a custom insecure TLS path. Configured names remain explicit DNS trust;
  an exact IP endpoint is the narrowest local-service grant.
- Reject userinfo, query, fragments, scoped addresses, opaque/non-HTTP URLs,
  malformed hosts/ports and whitespace/control characters. Canonical endpoint
  matching must not ignore ports, paths or escaped-path distinctions.
- A single five-minute request context covers DNS, dial, TLS, headers and body,
  bounded by the caller's earlier deadline/cancellation. Responses are at most
  10 MiB; an extra byte is a refusal, not silently truncated success. Errors
  expose static classes/status codes, never raw network errors, URLs, server
  bodies, prompts or credentials.
- Policy is immutable at executor construction, just like the HTTP policy.
  Missing means public-only; unreadable, malformed or oversized means refusal.
  The file uses a closed YAML schema, at most 64 exact endpoints and 16 KiB;
  duplicate/unknown keys, aliases and extra documents are invalid. No file is
  written. Both worker construction sites use the real node config directory.

The existing default endpoint is `http://127.0.0.1:11434/api/chat`. Under the
new policy it is intentionally refused until the node operator explicitly
grants it. This is a security behavior change, not proof that a default local
workflow remains compatible. Before any future authorized rollout, review that
change and let the operator prepare the policy through the normal configuration
process. This source work does not alter a real node or grant itself an endpoint.

Proposed explicit operator configuration (documentation only):

```yaml
allow:
  - endpoint: http://127.0.0.1:11434/api/chat
```

This grants only that endpoint to LLM nodes, not `/api/delete`, another port,
the workflow HTTP node or arbitrary loopback services. It does not bypass the
workflow's existing caller authorization or Shell gate, grant inference/model
execution in this development session, or establish model/server trust.

## Phased source work

1. Publish this full plan first. Audit existing LLM request/response behavior,
   both executor construction sites and shared HTTP address/transport rules.
   Keep the source/compatibility policy explicitly review-required.
2. Implement the machine-local policy parser/loader and exact URL resolution,
   preserving hard-denial and every-answer DNS checking. Prepare hermetic
   malformed-config, endpoint-confusion, range and rebinding regressions.
3. Wire the actual LLM executor and both worker entrypoints. Apply bounded
   request/response and static-error behavior while retaining the existing
   Ollama request/success-response schema. Update its mock-server test to use
   the guarded production path rather than a default-client localhost bypass.
4. Prepare request/redirect/proxy/TLS/cancellation/deadline/body-limit/privacy
   and real-construction tests, plus source-only operator migration notes.
   Obtain normal exact-head CI and independent source/security review. Do not
   assign historical dependency CI to the composed/current head.

## Completion and holds

The issue is not fixed by a parser/helper that the actual LLM executor does not
use. Both live construction sites and a zero-value executor must enforce the
policy, and malformed configuration must never silently restore an unrestricted
client. The HTTP node's security posture must remain unchanged.

Only existing normal GitHub CI may execute this candidate in this lane. Local
tests/build/formatter, real network services or LLM calls, model downloads,
GPU/inference, policy/credential changes and deployment remain held. Source
approval, compatibility ratification, readiness and native acceptance are
separate; no automatic merge or node configuration step is included.
