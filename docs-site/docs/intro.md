---
sidebar_position: 1
slug: /
title: Citadel CLI
---

# Citadel CLI

Citadel is the on-premise agent that connects your hardware to the AceTeam Sovereign Compute Fabric. Install it on any machine with a GPU, join the AceTeam Network, and start running AI workloads -- all while keeping your data on infrastructure you control.

## Why Citadel?

**Sovereign.** Your data never leaves your hardware. AI inference runs locally on your machines, not in someone else's data center. Meet compliance requirements without sacrificing capability.

**Simple.** Two commands to go from bare metal to production-ready AI node. No complex networking configuration, no cloud console, no Kubernetes expertise required.

**Secure.** Every connection is encrypted end-to-end through a secure mesh network. No inbound ports to open, no VPN appliances to manage, no attack surface to worry about.

## Get Started in 2 Commands

```bash
citadel init
citadel work
```

That is all it takes to connect your hardware and start accepting AI workloads from the AceTeam platform.

## Documentation Guide

This documentation is organized into tiers based on your role and what you need to know.

**[Overview](/overview/what-is-citadel)** -- Start here if you are evaluating AceTeam or need business context. Covers what Citadel is, how it fits into the broader platform, and real-world use cases.

**[Getting Started](/getting-started/installation)** -- For operators and engineers setting up nodes. Step-by-step installation, first-run walkthrough, and full provisioning options.

**[Guides](/guides/managing-services)** -- Hands-on guides for day-to-day operations: managing inference services, monitoring node health, networking configuration, and automation.

**[Architecture](/architecture/overview)** -- For engineers who want to understand how the system works under the hood. Covers the mesh network, job processing pipeline, status reporting, and key design decisions.

**[Development](/development/contributing)** -- For contributors to the Citadel CLI codebase. Project structure, how to add new job handlers, testing strategy, and release process.

**[Reference](/reference/commands)** -- Complete command reference, configuration file format, and glossary of terms.
