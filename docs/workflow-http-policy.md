# Workflow HTTP destination policy

Workflow HTTP nodes are public-only unless the node operator provides an
exception in `workflow-http-policy.yaml` in the node configuration directory.
`network.GetNodeConfigDir` owns that directory's location. The worker and control
center both load the same policy when constructing their workflow executor.
The file is not part of a workflow graph, run input or remotely supplied manifest.

Example, explicitly authorizing one local subnet and one exact host:

```yaml
allow:
  - cidr: 192.168.2.0/24
  - host: nas.internal
```

An exact host exception allows that host's private/reserved DNS answers, not
arbitrary hosts in the same DNS suffix. CIDR exceptions must be contained in a
private/reserved range, not a public subnet or a broad default route. IPv4-mapped
and compatible IPv6 addresses are classified as their IPv4 destination.
Loopback, unspecified, link-local/metadata, multicast, broadcast, IPv6 site-local
and the node-local network DNS address are always refused, even with an exception.
All DNS answers must pass; mixed public/private answers are refused unless every
private answer has a corresponding operator exception. A lookup is performed
once per request and the connection is pinned to the validated numeric address.
The original hostname remains the HTTP Host and HTTPS certificate/SNI name.

Missing policy means public-only. Malformed, oversized, non-regular or unreadable
policy refuses HTTP nodes rather than widening access. Policy files must have
unique keys, known fields and no YAML aliases. The policy is a bounded snapshot;
editing it requires rebuilding/restarting the executor to take effect. No node
file is created or modified by loading the policy.

Only HTTP and HTTPS URLs without user credentials are accepted. Redirects are
not followed: the node returns the original 3xx status and a `location` field.
An environment proxy cannot redirect the connection. The request timeout covers
resolution, connection and reading: the default is 30 seconds; nonpositive values
use that default and positive values are capped at 120 seconds. Response bodies
retain the existing 10 MiB read cap.

Refusal errors report the blocked address class, not resolved private addresses.
Debug logs include the requested hostname and class. HTTP policy does not change
LLM endpoint policy or the separate job HTTP proxy, both of which have different
contracts. Test-only resolver/dial seams exercise this guard using owned mock
servers; production has no localhost bypass.
