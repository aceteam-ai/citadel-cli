# Service job policy

Dispatched service start, stop and status operations use this node's configured
organization and node identity and its own service control stream. Local service
operations are unchanged. The worker reads the current local `services`
permission for each dispatch; unreadable or malformed policy is refused.

Service requests using host networking also require `host_network: true` in the
node's local `permissions.yaml`. This permission defaults to false, including on
existing installations that omit the field. Standard container networking does
not need this additional permission. Changes take effect on the next request.

The worker and control-center worker use the same service policy wiring.
