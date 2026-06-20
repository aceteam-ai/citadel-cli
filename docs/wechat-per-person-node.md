# WeChat: per-person microservice on a dedicated node

WeChat accounts are personal — one per person, there is no shared org WeChat. So
the WeChat microservice ([`sunapi386/wechat`](https://github.com/sunapi386/wechat):
a FastAPI `:8000` server wrapping WeChatFerry) runs **per person on that
person's own Citadel node**, and the backend routes that person's `wechat_*` MCP
calls to the node that actually has their WeChat.

This document describes the citadel-cli side of that flow. The backend binding
and node-targeted dispatch live in the aceteam repo (see
`aceteam-ai/aceteam#4028` and the design doc
`docs/plans/2026-06-20-wechat-per-person-node-binding.md`).

## What citadel-cli does (and does not) do

The Citadel worker (`citadel work`) already subscribes to its **per-node shell
stream** and dispatches jobs by `type` type-agnostically, so the backend's
`HTTP_PROXY` job is relayed to the local `:8000` by whichever node consumes that
node's stream. **No worker-side change and no service registration is required**
for WeChat calls to reach the VM.

What the catalog/CLI adds is **discoverability**:

- The `wechat` entry in the [citadel-services](https://github.com/aceteam-ai/citadel-services)
  catalog documents the port (`8000`), health endpoint (`/health`), and auth env
  var (`WCF_API_KEY`). Browse it with `citadel service catalog info wechat`.
- `wechat` is included in the `citadel expose`, `citadel connect`, and
  `citadel proxy` service maps so operators can see/reach `:8000`.

> **`wechat` is not installable via the catalog.** WeChatFerry is Windows-only
> DLL injection into the running WeChat desktop client, so there is no
> `compose.yml` and nothing to `docker compose up`. `citadel service catalog
> install wechat` deliberately prints provisioning guidance instead of
> attempting a container install. Provision it with the PowerShell scripts in
> the upstream repo.

`citadel expose 8000` is a **display** command (it prints this node's network
access info / peer service URLs); it does not advertise or register a service
into a routing fabric. The enablement flow does not depend on `expose`; it
depends on (a) the node's worker being subscribed to its per-node stream and
(b) the person running `wechat_connect(..., node_id)`.

## Per-person enablement flow

1. **Provision the VM** — run `provision/bootstrap.ps1` (admin PowerShell) on a
   Windows 10/11 VM. Installs WeChat 3.9.12.51, Python/uv, Defender exclusions,
   firewall (only `:8000` open, RPC `10086-10087` blocked), and generates
   `WCF_API_KEY` into `api/.env`.
2. **QR login** — scan the WeChat QR on the PVE console / RDP, confirm on phone
   (2FA every restart; login does not persist).
3. **Start the service** — `provision/start-service.ps1` runs uvicorn on `:8000`.
4. **Node is ready to relay** — the Citadel worker on the node co-located with
   the VM is already subscribed to its per-node shell stream, so it can relay
   `HTTP_PROXY` to the VM over the LAN. (`citadel expose 8000` optionally prints
   the node's access info for the operator, but is not required for routing.)
5. **Backend binds + routes** — the person calls
   `wechat_connect(api_url, api_key, node_id)` (their node's Headscale numeric
   ID, from `terminal_list_nodes`). The backend stores the binding keyed to the
   person and routes every `wechat_*` call to that node's per-node stream — so
   the call is always relayed from the machine that actually has their WeChat.
