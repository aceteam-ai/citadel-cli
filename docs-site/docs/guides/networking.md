---
sidebar_position: 3
title: Networking
---

# Networking

All nodes on the AceTeam Network can communicate with each other through an encrypted mesh network. No public internet exposure is required -- traffic stays within the fabric, encrypted end-to-end.

## Service Discovery

List all nodes on your network and their capabilities:

```bash
citadel peers
```

This queries the network for connected nodes and displays their names, IP addresses, and available services. Use it to find which nodes are online and what they can do.

You can also use the older command:

```bash
citadel nodes
```

## Direct Calls

Make HTTP requests to services running on other nodes:

```bash
citadel call <node> <endpoint>
```

For example, to call an Ollama endpoint on a peer node:

```bash
citadel call gpu-server-01 /api/generate
```

Requests are routed through the mesh network. You do not need to know the node's IP address -- Citadel resolves it by name.

## SSH Access

SSH into any node on the network:

```bash
citadel ssh <node>
```

This opens an SSH session to the named node through the mesh network. No port forwarding or public IP is required.

## Ping

Check if a peer node is reachable:

```bash
citadel ping <node>
```

This sends an HTTP-level ping through the mesh (not ICMP) and reports the round-trip time.

## Service Exposure

Expose a local port to other nodes on the fabric:

```bash
citadel expose 5432
# Other nodes can now reach your PostgreSQL at <your-node-ip>:5432
```

Run it with no arguments to see this node's network IP and the URLs peers can
use, `--peers` to see what other nodes are offering, and `--check` to verify
that the services are actually reachable:

```bash
citadel expose            # this node's network IP and service URLs
citadel expose --peers    # services offered by other nodes
citadel expose --check    # verify reachability
```

This makes the specified port on your node accessible to other fabric members through the mesh network. Any TCP service (databases, APIs, custom servers) can be exposed.

## HTTP Proxy

Proxy local traffic to a service on a remote node:

```bash
citadel proxy 11434 gpu-server-01:11434
# Access remote Ollama as if it were local: http://localhost:11434
```

The first argument is the local port, the second is `<peer>:<port>` (the peer
can be a node name or a network IP). Run `citadel proxy` with no arguments for
an interactive prompt. The proxy runs until interrupted with Ctrl+C.

This sets up a local proxy that forwards requests to a remote node's services, allowing you to access them as if they were running locally.

### Expose vs. Proxy

| | `citadel expose` | `citadel proxy` |
|---|---|---|
| **Direction** | Makes your local service available to others | Makes a remote service available to you |
| **Use when** | You run a service others need to reach | You need to access a service on another node |
| **Port** | Listens on the mesh network interface | Listens on localhost |
| **Example** | Expose a database for other nodes | Access a remote GPU node's inference API locally |

## Reaching a Peer From Your Own Shell

Citadel's own commands (`citadel ssh`, `citadel call`, `citadel ping`,
`citadel proxy`) route over the mesh with no extra setup, because the network
client is built into the binary. You do not need to install Tailscale or any
other VPN app to use them.

That membership is scoped to the Citadel process, not to your whole machine.
So an unrelated host tool -- a plain `ssh`, `curl`, `psql`, or your browser --
will not resolve a peer's network address on its own. Bring the port to
localhost first:

```bash
# Make the peer's port 22 available at localhost:2222, then use plain ssh.
citadel proxy 2222 gpu-server-01:22
ssh -p 2222 user@localhost
```

`citadel proxy` is the supported answer whenever a non-Citadel program needs to
talk to a peer service. (Machine-wide routing, which would let any program on
the host address peers directly, is in development.)

## Security Model

- All inter-node traffic is encrypted end-to-end through the secure mesh network.
- No inbound ports need to be opened on your firewall.
- Nodes are not exposed to the public internet.
- Communication is restricted to nodes that have been authorized on your AceTeam Network.

### Access Control and Audit

Expose and proxy commands operate within the mesh network's access control layer:

- **ACL enforcement.** The coordination server defines access control lists that determine which nodes can reach which services. Expose and proxy respect these rules -- a node cannot expose a port to nodes that are not authorized to access it.
- **Audit logging.** All connections through the mesh are traceable. The coordination server logs which nodes communicate and when, providing a full audit trail.
- **Config-based disable.** Administrators can disable expose and proxy functionality via node configuration to enforce stricter network policies.

```
Traffic flow:
  Node A (expose :5432)  ──[WireGuard tunnel]──>  Node B (proxy localhost:5432)
                              │
                         Coordination Server
                         (ACL check + audit log)
```

All traffic between nodes is encrypted by the WireGuard protocol. There is no point in the path where data is transmitted in plaintext.
