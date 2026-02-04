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
citadel expose --port 5432
# Other nodes can now reach your PostgreSQL at <your-node-ip>:5432
```

This makes the specified port on your node accessible to other fabric members through the mesh network. Any TCP service (databases, APIs, custom servers) can be exposed.

## HTTP Proxy

Proxy local traffic to a service on a remote node:

```bash
citadel proxy gpu-server-01 --port 11434 --local-port 11434
# Access remote Ollama as if it were local: http://localhost:11434
```

This sets up a local proxy that forwards requests to a remote node's services, allowing you to access them as if they were running locally.

### Expose vs. Proxy

| | `citadel expose` | `citadel proxy` |
|---|---|---|
| **Direction** | Makes your local service available to others | Makes a remote service available to you |
| **Use when** | You run a service others need to reach | You need to access a service on another node |
| **Port** | Listens on the mesh network interface | Listens on localhost |
| **Example** | Expose a database for other nodes | Access a remote GPU node's inference API locally |

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
