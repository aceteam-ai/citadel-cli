---
sidebar_position: 5
title: Machine-Wide Network Mode
---

# Machine-Wide Network Mode

By default, joining the AceTeam Network (`citadel init` / `citadel login`)
connects the **Citadel process only** -- no sudo required, no system-level
network changes. Other programs on the machine (a plain `ssh`, `curl`, your
browser) can't resolve a peer's network address directly; you route through
`citadel proxy` for that (see [Networking](./networking.md)).

`citadel up` goes further: it puts the **whole machine** on the network via a
real network interface, so any program on the box -- SSH, a browser, a
database client -- can reach other nodes directly.

## When you need it

Use `citadel up` when a tool you don't control needs mesh access and can't be
routed through `citadel proxy`:

- A GUI application that needs to reach a peer's IP directly.
- A script or service that expects the network to just be there, with no
  Citadel-specific plumbing.
- Ad-hoc debugging where you want `ping`, `curl`, or a browser to reach a
  peer's network address exactly like a LAN host.

If you only need Citadel's own commands (`ssh`, `call`, `proxy`, `expose`) or
a running worker, you don't need this -- the default per-process mode already
covers those with no elevated privileges.

## Turning it on

```bash
sudo citadel up
```

This requires root/administrator privileges (it configures real system
routing and DNS) and is strictly opt-in. It runs in the foreground; leave the
terminal open, or run it under a supervisor if you want it to persist across
reboots.

Take the machine back off the network:

```bash
sudo citadel down
```

This reverses `citadel up` -- restoring your normal routing and DNS -- without
touching your `citadel.yaml` manifest or stopping any services.

## Checking compatibility first

Some environments already run other VPN software that machine-wide mode needs
to coexist with. Verify it will work on this machine without actually staying
connected:

```bash
sudo citadel up --check
```

This creates the network interface, confirms it works, and immediately tears
it down again -- no routes installed, no DNS changes, safe to run on a
production box or under a remote script that might be interrupted midway.

## One machine, one node

`citadel up` and `citadel login`/`citadel work` share the same node identity.
If a worker is already running in per-process mode and you bring up
machine-wide mode on the same box, the worker automatically attaches to the
existing machine-wide connection instead of opening a second, redundant
connection to the network. You never end up with two separate node identities
on one machine.

## Platform notes

- **Linux and macOS:** fully supported.
- **Windows:** fully supported. If another VPN client is already installed
  and running, Citadel's machine-wide mode is designed to install alongside
  it without colliding, though running two VPN products that both want to
  manage overlapping network ranges is worth testing in your environment
  before relying on it in production.
