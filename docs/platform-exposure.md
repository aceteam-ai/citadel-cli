# Platform mesh exposure

`citadel service expose <name> --port <port> --visibility platform` publishes a
TCP listener on this node's own mesh IP at the requested port. It forwards bytes
to `127.0.0.1:<port>` by default, preserving HTTP Host, cookies, upgrades, and
other protocol bytes. Use `--forward-target <private-IP>:<port>` if the local
service listens elsewhere. The target must be a numeric loopback or private IP.
The published mesh port remains the value passed to `--port`.

Each connection is admitted only when its verified mesh identity has the same
owner as this node, or its login is listed in the node's
`platform-exposure.json` file. Place that file in the directory returned by
the node's `GetNodeConfigDir` resolution, beside the node configuration:

```json
{"allowed_logins":["infra@example.test"]}
```

The file is optional; an absent file allows same-owner peers only. A malformed
or unreadable file denies all connections. Changes take effect on the next
connection. The allowlist is node configuration and cannot be set by an
`EXPOSE_SET` payload. A port collision fails the expose operation. In userspace
mesh mode low ports need no host privilege; kernel TUN and attached modes may
require `CAP_NET_BIND_SERVICE` for ports below 1024.

`EXPOSE_LIST` reports the listener and its target, and `UNEXPOSE` closes it and
removes its durable record. The listener is restored from that record when the
worker starts again. This is a raw TCP path and does not use the gateway's
`/expose/<name>/` URL.
