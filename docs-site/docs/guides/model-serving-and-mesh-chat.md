---
sidebar_position: 2
title: Model Serving & Mesh Chat
---

# Model Serving & Mesh Chat

Once your node is online, you can serve a model locally and talk to any
model running on *any* node in your fleet -- discovered automatically, with
no manual IP bookkeeping.

## Serving a Model

The AceTeam platform can deploy and manage models on your node remotely (see
the [AceTeam Fabric console](https://aceteam.ai/fabric)). You can also start
an inference engine directly from the CLI:

```bash
citadel run vllm       # or: ollama, llamacpp
```

This starts the engine as a managed service using your `citadel.yaml`
manifest. See [Managing Services](./managing-services.md) for the full
lifecycle (stop, restart, logs, diagnostics).

Once a service is running, `citadel status` shows what's serving and on
which port:

```bash
citadel status
```

## Discovering Models Across the Fleet

Every node running `citadel work` (the default) advertises the models it
serves. From any other node on the network, list everything currently
available:

```bash
citadel mesh models
```

```
MODEL                NODE           ENGINE   ADDRESS
-----                ----           ------   -------
Qwen/Qwen2.5-7B       gpu-node-1     vllm     100.64.0.12:8443
llama3.1:8b           gpu-node-2     ollama   100.64.0.14:8443
```

Add `--json` to include unreachable nodes and why they couldn't be probed --
useful for scripting or troubleshooting a node that isn't showing up.

```bash
citadel mesh models --json
```

## Chatting With a Remote Model

Send a one-shot chat request to a model on another node, without knowing its
address:

```bash
citadel mesh chat --model Qwen/Qwen2.5-7B "Summarize this in one sentence."
```

Selection rules:

- If `--model` uniquely identifies one served model anywhere on the mesh,
  `--node` is optional.
- If the same model is served on more than one node, add `--node <name-or-ip>`
  to pick which one.
- If a node serves exactly one model, `--model` is optional and `--node`
  alone is enough.

```bash
# Pick a specific node when a model is served in several places
citadel mesh chat --node gpu-node-1 --model llama3.1:8b "hello"

# The node serves only one model -- no --model needed
citadel mesh chat --node gpu-node-1 "hello"
```

This is a minimal, non-interactive command -- good for scripts and quick
checks. It sends a single request and prints the reply; there's no
multi-turn session state.

### Reachability note

Discovery and chat both rely on the target node running with its gateway
enabled, which is `citadel work`'s default. A node started with
`--no-gateway` won't be visible to `citadel mesh models`, since it isn't
advertising its status over the network.

## Why This Matters

You get one fleet-wide view of "what models are available and where" without
maintaining a registry, an IP address book, or a load balancer. Add a new
node with a new model, and it's discoverable and chat-able from every other
node within seconds -- the same zero-configuration principle that governs the
rest of the network (see [How It Works](../overview/how-it-works.md)).
