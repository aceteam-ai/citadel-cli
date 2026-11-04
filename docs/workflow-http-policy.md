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
and both node-local network DNS addresses (`100.100.100.100` and
`fd7a:115c:a1e0::53`) are always refused, even with an exception.

Translated/tunneled IPv6 prefixes are also non-allowlistable: NAT64
`64:ff9b::/96` and `64:ff9b:1::/48`, 6to4 `2002::/16`, Teredo
`2001::/32`, and SIIT translated `::ffff:0:0:0/96`. This includes embedded
public IPv4 destinations; a host exception cannot turn them on. DNS64-only
networks using those NAT64 prefixes may therefore refuse otherwise public sites.
Use ordinary public IPv4/native IPv6 connectivity for HTTP nodes. Custom
translation outside these known prefixes is not detected automatically; local
network routing remains an operator trust boundary. Normal IPv4-mapped and
compatible addresses still receive the embedded IPv4 classification.
Prefix references: [IANA special-purpose registry](https://www.iana.org/assignments/iana-ipv6-special-registry/),
[RFC 6052](https://www.rfc-editor.org/rfc/rfc6052.html) and
[RFC 2765 section 2.1](https://www.rfc-editor.org/rfc/rfc2765.html#section-2.1).

All DNS answers must pass; mixed public/private answers are refused unless every
private answer has a corresponding operator exception. A lookup is performed
once per request and the connection is pinned to the validated numeric address.
The original hostname remains the HTTP Host and HTTPS certificate/SNI name.

Missing policy means public-only. Malformed, oversized, non-regular or unreadable
policy refuses HTTP nodes rather than widening access. Policy files must have
unique keys, known fields and no YAML aliases. The policy is a bounded snapshot;
editing it requires rebuilding/restarting the executor to take effect. No node
file is created or modified by loading the policy.
Construction logs one warning per executor with a bounded load-failure reason
(for example `open_permission_denied`, `file_not_regular`, `file_too_large`, or
`invalid_policy_document`). It does not log raw YAML or filesystem paths. A
missing policy produces no warning; request errors retain a generic refusal.

Only HTTP and HTTPS URLs without user credentials are accepted. Redirects are
not followed: the node returns the original 3xx status and a `location` field.
An environment proxy cannot redirect the connection. The request timeout covers
resolution, connection and reading: the default is 30 seconds; nonpositive values
use that default and positive values are capped at 120 seconds. Response bodies
retain the existing 10 MiB read cap.

Hostname labels use ASCII letters, digits and hyphens only, with no leading or
trailing hyphen. Underscores and raw Unicode IDN names are rejected in URLs and
host exceptions. An operator can supply the corresponding ASCII punycode
A-label, such as `xn--bcher-kva.example`; no implicit IDNA conversion is done.
This deliberately preserves the existing charset restriction rather than
silently changing DNS and TLS names for existing graphs.

Refusal errors report the blocked address class, not resolved private addresses.
Request and response-read transport errors also omit underlying addresses,
ports, queries and certificate details; cancellation and deadline identity are
retained for callers using `errors.Is`.
Debug logs include the requested hostname and class. HTTP policy does not change
LLM endpoint policy or the separate job HTTP proxy, both of which have different
contracts. Test-only resolver/dial seams exercise this guard using owned mock
servers; production has no localhost bypass.
