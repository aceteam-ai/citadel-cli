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

Expose local services to other nodes on the fabric:

```bash
citadel expose
```

This makes services running on your node accessible to other fabric members through the mesh network.

## HTTP Proxy

Proxy local traffic to a service on a remote node:

```bash
citadel proxy <node>
```

This sets up a local proxy that forwards requests to a remote node's services, allowing you to access them as if they were running locally.

## Security Model

- All inter-node traffic is encrypted end-to-end through the secure mesh network.
- No inbound ports need to be opened on your firewall.
- Nodes are not exposed to the public internet.
- Communication is restricted to nodes that have been authorized on your AceTeam Network.
