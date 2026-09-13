# Dual-public-IP egress CI runner

This repo has one automated test that **cannot run on a normal CI runner**: the
proof that traffic tunneled through the [on-node egress relay](../internal/egressrelay)
(#787 / #980, relay-only `serve` mode #1037/#1033) leaves the fabric from a
**different public IP** than the client's own uplink. A GitHub-hosted runner has
a single uplink and a single NAT, so "the egress IP actually changed" is not
observable there. It is only meaningful — and only testable — on a fabric with
two independent public egresses.

The [`.github/workflows/egress-e2e.yml`](../.github/workflows/egress-e2e.yml)
workflow drives that test nightly against the aceteam dual-uplink lab.

## Topology

The Proxmox host `citadel3090` (`ssh root@192.168.2.4`) has **two distinct
internet uplinks** and one LXC pinned to each:

| Role | CT | Uplink | LAN | Mesh IP | citadel unit |
|---|---|---|---|---|---|
| **Relay** | CT114 `citadel-relay` | Cogeco cable | 192.168.0.11 | `100.64.0.53` | `citadel-relay.service` → `citadel egress-relay serve` (mesh SOCKS5 `:7861`) |
| **Client** | CT113 `citadel-client` | Bell fiber | 192.168.2.78 | `100.64.0.55` | `citadel-proxy.service` → `citadel proxy 1055 100.64.0.53:7861 --bind 127.0.0.1` (local SOCKS5 `127.0.0.1:1055` → relay mesh) |

The client's `citadel proxy` bridges a **local** SOCKS5 port to the relay's mesh
endpoint using citadel's own userspace mesh dial, so **no kernel TUN is required
on the client**. The self-hosted GitHub Actions runner lives *inside CT113*, so
from the runner's perspective:

- `curl https://api.ipify.org` → **Bell** (the client's own uplink)
- `curl --socks5-hostname 127.0.0.1:1055 https://api.ipify.org` → **Cogeco** (through the relay)

The [`scripts/egress-relay-ci.sh`](../scripts/egress-relay-ci.sh) canary asserts
those two public IPs differ. It never hardcodes an ISP IP, so residential-IP
churn does not cause a false red.

## What the workflow runs

Two independent jobs (see the workflow header for the full rationale):

- **`egress-unit`** — hosted `ubuntu-latest`, hermetic. `go build ./...` + the
  egress-relay/config/CLI unit tests. Catches a code regression even if the lab
  is offline.
- **`egress-liveness`** — `runs-on: [self-hosted, dual-ip-egress]` (CT113). Runs
  the canary script. This is the piece that leverages both public IPs.

**Trigger is the trust boundary.** Because the self-hosted runner sits on a home
LAN, the workflow has **no `pull_request` trigger** — a fork PR must never make
the LAN runner execute its code. It runs on a nightly `schedule` (always trusted
`main`) and on manual `workflow_dispatch` only. If you ever want a PR signal,
add it behind a GitHub **Environment protection rule** requiring manual
approval, not a bare `if:` expression (an `if:` evaluates *after* the runner has
already picked up the fork's workflow file).

## Runner install (inside CT113, reproducible)

Run once. The runner registers with label `dual-ip-egress`, which is the only
thing `egress-liveness` targets.

```bash
# 1. Mint a short-lived registration token (needs repo admin; from a machine
#    with gh authed to aceteam-ai):
gh api -X POST /repos/aceteam-ai/citadel-cli/actions/runners/registration-token --jq .token

# 2. On CT113 (via `pct exec 113` from the pve host, or SSH into the CT):
#    git is required by actions/checkout; the box ships curl+tar but not git.
apt-get update && apt-get install -y git

#    Dedicated non-root user (the Actions runner refuses to run as root):
useradd -m -s /bin/bash ghrunner || true

#    Install the runner as ghrunner:
sudo -u ghrunner bash -lc '
  set -e
  cd ~ && mkdir -p actions-runner && cd actions-runner
  RUNNER_VERSION=2.319.1   # bump to the current actions/runner release
  curl -fsSL -o r.tar.gz https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz
  tar xzf r.tar.gz && rm r.tar.gz
  ./config.sh --unattended \
    --url https://github.com/aceteam-ai/citadel-cli \
    --token <REGISTRATION_TOKEN_FROM_STEP_1> \
    --name ct113-bell-egress \
    --labels dual-ip-egress \
    --work _work
'

# 3. Install + start it as a systemd service (survives reboots):
cd /home/ghrunner/actions-runner
./svc.sh install ghrunner
./svc.sh start
./svc.sh status
```

The registration token expires ~1 hour after minting; re-mint if `config.sh`
rejects it. To replace the runner later, `./config.sh remove --token <REMOVE_TOKEN>`
(mint a *remove* token the same way) then re-run step 2.

## Mesh auth: persisted state + monthly key refresh

Both CTs are joined to the mesh with **persistent node identities**, so the
canary does **not** re-authenticate per run — it just uses the already-running
`citadel-relay` / `citadel-proxy` units. No secret is needed for the normal path.

The only time a fresh key is needed is if a CT's node key *expires* and it drops
off the mesh. Keep a **reusable** preauth key handy for that re-join:

```bash
# Mint a reusable 168h key (Headscale max), same org as the CTs:
#   (via the aceteam MCP `nexus_generate_authkey`, reusable=true, expiration_hours=168)
# Then, on a dropped CT:
systemctl stop citadel-relay   # or citadel-proxy
citadel login --authkey tskey-... --nexus https://nexus.aceteam.ai
systemctl start citadel-relay
```

Refresh cadence: a reusable Headscale preauth key maxes out at 168h (7 days), so
if you store one as a fallback, treat re-minting as a **monthly runbook step**,
not automation. Do **not** store a Headscale *admin* API key on the runner — the
persisted-state path means the runner never needs to mint keys itself.

## Optional hard-IP checks (repo variables)

By default the canary only asserts `direct != relayed` (drift-proof). To also
pin the exact IPs, set repo **variables** (Settings → Secrets and variables →
Actions → Variables):

| Variable | Meaning | Default |
|---|---|---|
| `EGRESS_EXPECT_RELAY_IP` | assert relayed egress == this | unset (skip) |
| `EGRESS_EXPECT_CLIENT_IP` | assert direct egress == this | unset (skip) |
| `EGRESS_PROXY_ENDPOINT` | override the client's local SOCKS5 endpoint | `127.0.0.1:1055` |
| `EGRESS_IP_ECHO_PRIMARY` | override the primary IP-echo URL | `https://api.ipify.org` |
| `EGRESS_IP_ECHO_SECONDARY` | override the fallback IP-echo URL | `https://ifconfig.me/ip` |

Set the two `EXPECT_*` values only if you want the stronger check and are
willing to update them when the ISP hands out a new address. As of the last
validation: client (Bell) `142.181.124.149`, relay (Cogeco) `24.141.120.77`.

## Troubleshooting (keyed to the script's exit codes)

| Exit | Meaning | First thing to check |
|---|---|---|
| `2` | Direct fetch failed — the **runner/CT itself** has no internet (not a relay problem) | `pct exec 113 -- curl -sS https://api.ipify.org` |
| `3` | Relay path unreachable | Is `citadel-relay.service` up on CT114 **and** `citadel-proxy.service` up on CT113? `systemctl status` both; `citadel status` on each shows mesh online |
| `1` | Egress did **not** change (direct == relayed), or an `EXPECT_*` mismatch | Relay may be misrouting, or both CTs are somehow on the same uplink; check the relay's `allow-lan` is `disabled` and the proxy points at `100.64.0.53:7861` |
| `0` | Pass — relayed left the fabric from a different public IP | — |

Manual one-shot (from the pve host), the same thing the canary automates:

```bash
pct exec 113 -- sh -c 'echo direct:  $(curl -s https://api.ipify.org); \
                       echo relayed: $(curl -s --socks5-hostname 127.0.0.1:1055 https://api.ipify.org)'
```

## Relation to the demo runbook and #1006

This is the automated form of **Act 4** of
[`docs/demo-sovereign-fabric-runbook.md`](demo-sovereign-fabric-runbook.md). The
earlier general-purpose two-host harness (#1006, `scripts/egress-relay-test.sh`,
login-per-run against arbitrary ephemeral hosts) remains useful for validating
the relay on a *fresh* pair of nodes; this workflow instead exercises the
*standing* dual-uplink lab on a schedule, which is what makes the cross-IP
assertion continuously meaningful.
