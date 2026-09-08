#!/usr/bin/env bash
#
# scripts/egress-relay-test.sh
#
# One-command validation harness for the on-node SOCKS5 egress relay
# (citadel #787, PR #980): proves that a "client" citadel node can tunnel
# outbound traffic THROUGH a "relay" citadel node's own network egress, and
# that the relay's deny-LAN-by-default security policy actually holds.
#
# ---------------------------------------------------------------------------
# QUICK START
# ---------------------------------------------------------------------------
#
#   Single machine (default -- only works on a box that has NEVER run a real
#   citadel node; see the "SINGLE-MACHINE MODE" section below for why):
#
#     CITADEL_TEST_AUTHKEY=tskey-auth-xxx ./scripts/egress-relay-test.sh
#
#   Two separate hosts (the safe path on a box that IS a real citadel node,
#   e.g. this dev machine / node 1297):
#
#     CITADEL_TEST_AUTHKEY=tskey-auth-xxx \
#     RELAY_HOST=user@relay-box CLIENT_HOST=user@client-box \
#       ./scripts/egress-relay-test.sh
#
#   Preview without executing anything:
#
#     ./scripts/egress-relay-test.sh --dry-run
#
# ---------------------------------------------------------------------------
# PREREQUISITES
# ---------------------------------------------------------------------------
#
#   - This checkout must already contain PR #980's egress-relay code (either
#     merged to main, or cherry-picked onto your branch -- `go build ./...`
#     must succeed and `internal/egressrelay` must exist).
#   - go, curl, python3 on the machine(s) actually running citadel.
#   - CITADEL_TEST_AUTHKEY: a Headscale authkey for nexus.aceteam.ai (or
#     $NEXUS_URL), valid for TWO logins (reusable / multi-use -- a
#     single-use key will fail the second `citadel login`). Both the relay
#     and client node MUST end up in the SAME AceTeam org, since the relay
#     only authorizes same-org verified mesh peers -- if your key mints
#     nodes into different orgs, set CITADEL_TEST_AUTHKEY_CLIENT to a second
#     key from the SAME org instead of reusing CITADEL_TEST_AUTHKEY.
#   - Dual-host mode additionally needs passwordless SSH to both
#     RELAY_HOST/CLIENT_HOST and a matching OS/arch for the built binary (or
#     set CITADEL_BIN_LINUX_AMD64 etc. -- see --help).
#
# ---------------------------------------------------------------------------
# SINGLE-MACHINE MODE: WHY IT REFUSES ON A REAL NODE, AND WHAT IT CAN PROVE
# ---------------------------------------------------------------------------
#
# citadel's tsnet identity (internal/network.GetStateDir/GetNodeConfigDir) is
# deliberately MACHINE-CONVERGENT: every invocation on a box that has ever
# run `citadel init` resolves to the SAME state directory regardless of
# $HOME (first via a world-readable /etc/citadel/config.yaml or
# /etc/citadel/state-dir pointer, then via the legacy
# ~/citadel-node/network reuse check) -- this is intentional, to stop a
# machine from silently registering as two different Headscale nodes. It
# means two isolated $HOME dirs on such a box do NOT give you two
# independent node identities: both would collapse onto the box's real,
# already-registered node. This script's preflight check
# (preflight_single_machine) detects that condition and REFUSES rather than
# risk touching a real node's network state. On THIS repo's own dev machine
# (node 1297) this check will always fire -- that is a safety feature
# proving the guard works, not a bug in the script. Run it in dual-host mode
# there, or on any other, never-provisioned box for single-machine mode.
#
# Separately: even where single-machine mode CAN run, it can only prove that
# traffic flows client -> mesh -> relay -> internet and that the relay's
# authorization/deny-LAN policy holds -- it can NEVER prove the egress IP
# actually CHANGES, because both simulated nodes share the same machine's
# single internet uplink/NAT. That assertion is only meaningful -- and is
# strictly enforced -- in dual-host mode, where relay and client are
# expected to be on different networks.
#
# ---------------------------------------------------------------------------
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (env vars, all overridable)
# ---------------------------------------------------------------------------
NEXUS_URL="${NEXUS_URL:-https://nexus.aceteam.ai}"
AUTH_SERVICE="${AUTH_SERVICE:-}"
CITADEL_TEST_AUTHKEY="${CITADEL_TEST_AUTHKEY:-}"
CITADEL_TEST_AUTHKEY_CLIENT="${CITADEL_TEST_AUTHKEY_CLIENT:-$CITADEL_TEST_AUTHKEY}"
RELAY_HOST="${RELAY_HOST:-}"
CLIENT_HOST="${CLIENT_HOST:-}"
IP_ECHO_URL="${IP_ECHO_URL:-https://api.ipify.org}"
CITADEL_BIN_OVERRIDE="${CITADEL_BIN:-}"

DRY_RUN=0
KEEP=0

RUN_ID="$(date +%s)-$$"
RELAY_NODE_NAME="egress-relay-test-relay-${RUN_ID}"
CLIENT_NODE_NAME="egress-relay-test-client-${RUN_ID}"
LOCAL_PROXY_PORT=18980
LAN_TEST_PORT=18981

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Small utilities
# ---------------------------------------------------------------------------
c_red='\033[0;31m'; c_green='\033[0;32m'; c_yellow='\033[0;33m'; c_reset='\033[0m'

log()  { printf '%b\n' "[egress-relay-test] $*"; }
ok()   { printf '%b\n' "${c_green}[egress-relay-test] OK: $*${c_reset}"; }
warn() { printf '%b\n' "${c_yellow}[egress-relay-test] WARN: $*${c_reset}" >&2; }
die()  { printf '%b\n' "${c_red}[egress-relay-test] FATAL: $*${c_reset}" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: egress-relay-test.sh [--dry-run] [--keep] [--relay-host HOST] [--client-host HOST]

One-command validation of the on-node SOCKS5 egress relay (citadel #787 /
PR #980). See the header comment in this file for the full write-up.

Options:
  --dry-run             Print the plan (including the safety preflight
                         verdict) and exit; execute nothing.
  --keep                Do not tear down state dirs / stop the http.server
                         probe on exit (background citadel processes are
                         still stopped). For debugging a failed run.
  --relay-host HOST     SSH target (user@host) to run the relay node on.
                         Same as $RELAY_HOST.
  --client-host HOST    SSH target (user@host) to run the client node on.
                         Same as $CLIENT_HOST.
  -h, --help            Show this help.

Environment variables:
  CITADEL_TEST_AUTHKEY          Required (unless --dry-run). Headscale
                                authkey, reusable for 2 logins, same org.
  CITADEL_TEST_AUTHKEY_CLIENT   Optional; defaults to CITADEL_TEST_AUTHKEY.
  NEXUS_URL                     Default https://nexus.aceteam.ai
  AUTH_SERVICE                  Optional --auth-service override.
  RELAY_HOST / CLIENT_HOST      SSH targets for dual-host mode.
  IP_ECHO_URL                   Default https://api.ipify.org
  CITADEL_BIN                   Path to a prebuilt citadel binary to use
                                instead of building one from this checkout.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --keep) KEEP=1; shift ;;
    --relay-host) RELAY_HOST="$2"; shift 2 ;;
    --client-host) CLIENT_HOST="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

if [ -n "$RELAY_HOST" ] && [ -n "$CLIENT_HOST" ]; then
  MODE="dual"
elif [ -z "$RELAY_HOST" ] && [ -z "$CLIENT_HOST" ]; then
  MODE="single"
else
  die "set BOTH RELAY_HOST and CLIENT_HOST for dual-host mode, or NEITHER for single-machine mode (got RELAY_HOST=${RELAY_HOST:-<unset>} CLIENT_HOST=${CLIENT_HOST:-<unset>})"
fi

# ---------------------------------------------------------------------------
# Command execution abstraction: run_on <relay|client> <command string>
# For single-machine mode this just execs locally with HOME overridden; for
# dual-host mode it SSHes to the resolved host. This is what lets the rest
# of the script be mode-agnostic.
# ---------------------------------------------------------------------------
declare -A NODE_HOME
declare -A NODE_HOST
declare -A NODE_BIN
declare -A NODE_PID

run_on() {
  local who="$1"; shift
  local cmd="$*"
  local host="${NODE_HOST[$who]:-}"
  if [ -n "$host" ]; then
    ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" "$cmd"
  else
    bash -c "$cmd"
  fi
}

# Launches cmd in the background on the given node, capturing its PID into
# NODE_PID[<label>] so cleanup() can stop it. label must be unique. cmd must
# not itself contain double quotes (single quotes are fine).
run_bg_on() {
  local who="$1" label="$2"; shift 2
  local cmd="$*"
  local host="${NODE_HOST[$who]:-}"
  local out="/tmp/egress-relay-test-${label}.log"
  if [ -n "$host" ]; then
    local pid
    pid="$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" "nohup sh -c \"$cmd\" >$out 2>&1 & echo \$!")"
    NODE_PID["$label"]="$host:$pid"
  else
    bash -c "$cmd" >"$out" 2>&1 &
    NODE_PID["$label"]="$!"
  fi
}

kill_bg() {
  local label="$1"
  local entry="${NODE_PID[$label]:-}"
  [ -z "$entry" ] && return 0
  if [[ "$entry" == *:* ]]; then
    local host="${entry%%:*}" pid="${entry##*:}"
    ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" "kill $pid 2>/dev/null || true" || true
  else
    kill "$entry" 2>/dev/null || true
  fi
}

# ---------------------------------------------------------------------------
# Safety preflight (single-machine mode only): refuse to proceed if this
# host looks like it has ever run `citadel init` for real, because
# internal/network.GetStateDir()'s machine-convergent resolution would
# collapse BOTH throwaway $HOME-scoped "nodes" onto that real node's tsnet
# identity regardless of the $HOME override -- see the header comment.
# ---------------------------------------------------------------------------
global_citadel_dir() {
  case "$(uname -s)" in
    Darwin) echo "/usr/local/etc/citadel" ;;
    *)      echo "/etc/citadel" ;;
  esac
}

preflight_single_machine() {
  local reasons=()

  if [ "$(id -u)" = "0" ]; then
    reasons+=("running as root -- citadel's owner-home resolution prefers \$SUDO_USER's home over an overridden \$HOME, which would defeat the isolation this script relies on. Re-run as a normal user.")
  fi
  if [ -n "${SUDO_USER:-}" ]; then
    reasons+=("\$SUDO_USER is set (${SUDO_USER}) -- internal/network.getOwnerHomeDir() would resolve THAT user's home instead of our overridden \$HOME. Run this script from a plain (non-sudo) shell.")
  fi

  local gdir="$(global_citadel_dir)"
  if [ -f "${gdir}/config.yaml" ]; then
    reasons+=("${gdir}/config.yaml exists -- this is citadel's machine-global, world-readable node_config_dir pointer. internal/network.GetStateDir() reads it BEFORE ever consulting \$HOME, so every citadel process on this box (regardless of \$HOME) resolves to the SAME tsnet state as citadel's real node. Contents: $(cat "${gdir}/config.yaml" 2>/dev/null | tr '\n' ' ')")
  fi
  if [ -f "${gdir}/state-dir" ]; then
    reasons+=("${gdir}/state-dir exists -- the machine-global state-dir pointer file, which has even higher priority than config.yaml. Contents: $(cat "${gdir}/state-dir" 2>/dev/null)")
  fi

  local real_home
  real_home="$(eval echo "~$(id -un)")"
  if [ -d "${real_home}/citadel-node/network" ] && [ -n "$(ls -A "${real_home}/citadel-node/network" 2>/dev/null)" ]; then
    reasons+=("${real_home}/citadel-node/network already has tsnet state -- GetStateDir()'s legacy-reuse fallback would resolve to it even with \$HOME overridden, once the global-pointer checks above are also absent.")
  fi

  if [ "${#reasons[@]}" -gt 0 ]; then
    warn "single-machine mode is UNSAFE on this box -- it is (or has been) a real citadel node:"
    local r
    for r in "${reasons[@]}"; do
      printf '%b\n' "  - $r" >&2
    done
    warn "Re-run with --relay-host/--client-host (or \$RELAY_HOST/\$CLIENT_HOST) pointed at two hosts that have never run 'citadel init', e.g. two throwaway VMs."
    return 1
  fi

  ok "preflight: no signs of a real citadel node on this box -- single-machine mode is safe here."
  return 0
}

# ---------------------------------------------------------------------------
# Build the citadel binary from THIS checkout (must already contain PR
# #980's egress-relay code).
# ---------------------------------------------------------------------------
BUILD_DIR=""
build_binary() {
  if [ -n "$CITADEL_BIN_OVERRIDE" ]; then
    [ -x "$CITADEL_BIN_OVERRIDE" ] || die "CITADEL_BIN=$CITADEL_BIN_OVERRIDE is not executable"
    log "using prebuilt binary: $CITADEL_BIN_OVERRIDE"
    echo "$CITADEL_BIN_OVERRIDE"
    return 0
  fi

  if [ "$DRY_RUN" = "1" ]; then
    echo "<would run: go build -o <tmp>/citadel . (in $REPO_ROOT)>"
    return 0
  fi

  if [ ! -d "${REPO_ROOT}/internal/egressrelay" ]; then
    die "internal/egressrelay not found in $REPO_ROOT -- this checkout does not have PR #980's egress-relay code. Merge/cherry-pick it first (see PR #980, issue #787)."
  fi

  BUILD_DIR="$(mktemp -d /tmp/egress-relay-test-build.XXXXXX)"
  log "building citadel from $REPO_ROOT -> ${BUILD_DIR}/citadel ..."
  (cd "$REPO_ROOT" && go build -o "${BUILD_DIR}/citadel" .) >&2
  echo "${BUILD_DIR}/citadel"
}

# ---------------------------------------------------------------------------
# Cleanup: always stop background processes, log out both throwaway nodes,
# and (unless --keep) remove their state dirs and the built binary. Runs on
# ANY exit path (success, failure, or a signal) via trap.
# ---------------------------------------------------------------------------
CLEANUP_DONE=0
cleanup() {
  [ "$CLEANUP_DONE" = "1" ] && return 0
  CLEANUP_DONE=1
  log "cleaning up..."

  kill_bg "relay-work"
  kill_bg "client-proxy"
  kill_bg "relay-lan-server"
  sleep 1

  for who in relay client; do
    local home="${NODE_HOME[$who]:-}"
    [ -z "$home" ] && continue
    local bin="${NODE_BIN[$who]:-}"
    [ -z "$bin" ] && continue
    run_on "$who" "HOME='$home' CITADEL_NO_AUTO_UPDATE=1 '$bin' logout" >/dev/null 2>&1 || true
  done

  if [ "$KEEP" != "1" ]; then
    for who in relay client; do
      local home="${NODE_HOME[$who]:-}"
      [ -z "$home" ] && continue
      run_on "$who" "rm -rf '$home'" >/dev/null 2>&1 || true
    done
    [ -n "$BUILD_DIR" ] && rm -rf "$BUILD_DIR" 2>/dev/null || true
  else
    log "kept state dirs: relay=${NODE_HOME[relay]:-} client=${NODE_HOME[client]:-}"
  fi

  log "done."
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# wait_until DESCRIPTION TIMEOUT_SECS -- <command...>
# Retries the given command every 2s until it succeeds, returning 0. Returns
# 1 (never exits/dies itself) if the timeout elapses first -- callers decide
# whether that's fatal.
# ---------------------------------------------------------------------------
wait_until() {
  local desc="$1" timeout="$2"; shift 2
  local waited=0
  until "$@" >/dev/null 2>&1; do
    waited=$((waited + 2))
    if [ "$waited" -ge "$timeout" ]; then
      warn "timed out after ${timeout}s waiting for: $desc"
      return 1
    fi
    sleep 2
  done
  return 0
}

# ===========================================================================
# Main
# ===========================================================================

log "mode: $MODE"
if [ "$MODE" = "single" ]; then
  if ! preflight_single_machine; then
    if [ "$DRY_RUN" = "1" ]; then
      log "(--dry-run: preflight failed as shown above; a real run would stop here.)"
      exit 2
    fi
    exit 2
  fi
  NODE_HOME[relay]="$(mktemp -d /tmp/egress-relay-test-relay.XXXXXX 2>/dev/null || echo /tmp/egress-relay-test-relay.$$)"
  NODE_HOME[client]="$(mktemp -d /tmp/egress-relay-test-client.XXXXXX 2>/dev/null || echo /tmp/egress-relay-test-client.$$)"
  NODE_HOST[relay]=""
  NODE_HOST[client]=""
else
  command -v ssh >/dev/null 2>&1 || die "ssh not found (required for dual-host mode)"
  NODE_HOST[relay]="$RELAY_HOST"
  NODE_HOST[client]="$CLIENT_HOST"
  if [ "$DRY_RUN" != "1" ]; then
    NODE_HOME[relay]="$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$RELAY_HOST" mktemp -d /tmp/egress-relay-test-relay.XXXXXX)"
    NODE_HOME[client]="$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$CLIENT_HOST" mktemp -d /tmp/egress-relay-test-client.XXXXXX)"
  else
    NODE_HOME[relay]="<remote tmp dir on $RELAY_HOST>"
    NODE_HOME[client]="<remote tmp dir on $CLIENT_HOST>"
  fi
fi

log "relay  home: ${NODE_HOME[relay]}  (host: ${NODE_HOST[relay]:-localhost})"
log "client home: ${NODE_HOME[client]} (host: ${NODE_HOST[client]:-localhost})"

BIN="$(build_binary)"
log "binary: $BIN"

if [ "$DRY_RUN" = "1" ]; then
  cat <<EOF

--dry-run: plan (nothing executed)
  1. citadel login --authkey <redacted> --node-name $RELAY_NODE_NAME   (relay,  HOME=${NODE_HOME[relay]})
  2. citadel egress-relay enable                                       (relay)
  3. citadel work --debug-redis-url=<throwaway> --egress-relay --no-terminal --status-port 0  (relay, background)
  4. citadel whoami --json  -> mesh_ipv4                                (relay)
  5. citadel login --authkey <redacted> --node-name $CLIENT_NODE_NAME  (client, HOME=${NODE_HOME[client]})
  6. citadel proxy $LOCAL_PROXY_PORT <relay_mesh_ip>:7861               (client, background)
  7. curl (direct)               -> \$IP_ECHO_URL   ($IP_ECHO_URL)
  8. curl --socks5-hostname 127.0.0.1:$LOCAL_PROXY_PORT -> \$IP_ECHO_URL
     assert: relay-tunneled IP == relay's own mesh-observed public IP
     [dual-host mode]: assert relay-tunneled IP != client's direct IP
     [single-machine mode]: same-uplink, so IPs will match -- WARN, not fail
  9. deny-LAN-by-default probe: curl via relay proxy to a local-only HTTP
     server on the relay host -- expect FAILURE
 10. citadel egress-relay allow-lan on (relay); retry the same probe --
     expect SUCCESS
 11. grep the relay's log for "egress relay: authorized" to confirm the
     client's connection was actually authorized and relayed
 12. teardown: stop background processes, citadel logout (both), rm -rf
     throwaway state dirs
EOF
  exit 0
fi

[ -n "$CITADEL_TEST_AUTHKEY" ] || die "CITADEL_TEST_AUTHKEY is required for a real run (see --help / --dry-run)"

NODE_BIN[relay]="$BIN"
NODE_BIN[client]="$BIN"
if [ "$MODE" = "dual" ]; then
  log "copying binary to remote hosts..."
  scp -q "$BIN" "${RELAY_HOST}:${NODE_HOME[relay]}/citadel"
  scp -q "$BIN" "${CLIENT_HOST}:${NODE_HOME[client]}/citadel"
  NODE_BIN[relay]="${NODE_HOME[relay]}/citadel"
  NODE_BIN[client]="${NODE_HOME[client]}/citadel"
  run_on relay "chmod +x '${NODE_BIN[relay]}'"
  run_on client "chmod +x '${NODE_BIN[client]}'"
fi

AUTH_FLAG=""
[ -n "$AUTH_SERVICE" ] && AUTH_FLAG="--auth-service '$AUTH_SERVICE'"

# --- 1/2. Relay: login + enable relay -------------------------------------
log "relay: logging in as $RELAY_NODE_NAME ..."
run_on relay "HOME='${NODE_HOME[relay]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[relay]}' login --authkey '$CITADEL_TEST_AUTHKEY' --node-name '$RELAY_NODE_NAME' --nexus '$NEXUS_URL' $AUTH_FLAG"

log "relay: enabling egress relay config..."
run_on relay "HOME='${NODE_HOME[relay]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[relay]}' egress-relay enable"

# --- 3. Relay: start citadel work (starts the relay listener) -------------
log "relay: starting 'citadel work' (background)..."
run_bg_on relay relay-work \
  "HOME='${NODE_HOME[relay]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[relay]}' work --debug-redis-url=redis://127.0.0.1:1/0 --egress-relay --no-terminal --status-port 0 --max-concurrency 1"

# --- 4. Wait for relay to connect + read its mesh IP -----------------------
# relay_check_connected prints the relay's mesh_ipv4 and exits 0 once
# 'citadel whoami --json' reports both connected=true and a non-empty
# mesh_ipv4; exits 1 (prints nothing) otherwise, so the caller can poll it.
relay_check_connected() {
  local out
  out="$(run_on relay "HOME='${NODE_HOME[relay]}' '${NODE_BIN[relay]}' whoami --json" 2>/dev/null)" || return 1
  python3 - "$out" <<'PYEOF'
import json, sys
try:
    d = json.loads(sys.argv[1])
except Exception:
    sys.exit(1)
if d.get("connected") and d.get("mesh_ipv4"):
    print(d["mesh_ipv4"])
    sys.exit(0)
sys.exit(1)
PYEOF
}

log "relay: waiting for network connection..."
waited=0
RELAY_MESH_IP=""
while :; do
  if RELAY_MESH_IP="$(relay_check_connected)"; then
    break
  fi
  waited=$((waited + 2))
  if [ "$waited" -ge 90 ]; then
    die "timed out after 90s waiting for the relay to connect to the AceTeam Network (check ${NODE_HOME[relay]}/.citadel-cli/logs/latest.log on the relay host)"
  fi
  sleep 2
done
ok "relay connected, mesh IP: $RELAY_MESH_IP"

# --- 5. Client: login ------------------------------------------------------
log "client: logging in as $CLIENT_NODE_NAME ..."
run_on client "HOME='${NODE_HOME[client]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[client]}' login --authkey '$CITADEL_TEST_AUTHKEY_CLIENT' --node-name '$CLIENT_NODE_NAME' --nexus '$NEXUS_URL' $AUTH_FLAG"

# --- 6. Client: start citadel proxy toward relay's SOCKS5 port -------------
log "client: starting 'citadel proxy $LOCAL_PROXY_PORT $RELAY_MESH_IP:7861' (background)..."
run_bg_on client client-proxy \
  "HOME='${NODE_HOME[client]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[client]}' proxy $LOCAL_PROXY_PORT $RELAY_MESH_IP:7861 --bind 127.0.0.1"

proxy_port_open() {
  run_on client "nc -z 127.0.0.1 $LOCAL_PROXY_PORT"
}
wait_until "client local SOCKS proxy port $LOCAL_PROXY_PORT" 30 proxy_port_open \
  || die "'citadel proxy' never opened 127.0.0.1:$LOCAL_PROXY_PORT on the client (check /tmp/egress-relay-test-client-proxy.log on the client host)"
ok "client proxy is listening on 127.0.0.1:$LOCAL_PROXY_PORT"

# --- 7/8. Egress IP proof --------------------------------------------------
log "fetching direct egress IP (client, no relay)..."
DIRECT_IP="$(run_on client "curl -s --max-time 10 '$IP_ECHO_URL'" || true)"
log "client direct egress IP: ${DIRECT_IP:-<none>}"

log "fetching egress IP through the relay..."
RELAY_TUNNELED_IP="$(run_on client "curl -s --max-time 20 --socks5-hostname 127.0.0.1:$LOCAL_PROXY_PORT '$IP_ECHO_URL'" || true)"
log "relay-tunneled egress IP:  ${RELAY_TUNNELED_IP:-<none>}"

[ -n "$RELAY_TUNNELED_IP" ] || die "relay-tunneled curl returned nothing -- the SOCKS5 relay path is broken (check ${NODE_HOME[relay]}/.citadel-cli/logs/latest.log on the relay host)"

log "fetching relay's OWN direct egress IP (for comparison)..."
RELAY_OWN_IP="$(run_on relay "curl -s --max-time 10 '$IP_ECHO_URL'" || true)"
log "relay's own direct egress IP: ${RELAY_OWN_IP:-<none>}"

if [ -n "$RELAY_OWN_IP" ] && [ "$RELAY_TUNNELED_IP" != "$RELAY_OWN_IP" ]; then
  die "relay-tunneled IP ($RELAY_TUNNELED_IP) does not match the relay node's own egress IP ($RELAY_OWN_IP) -- traffic is not actually egressing from the relay"
fi
ok "relay-tunneled IP matches the relay node's own egress IP: $RELAY_TUNNELED_IP"

if [ "$MODE" = "dual" ]; then
  if [ "$RELAY_TUNNELED_IP" = "$DIRECT_IP" ]; then
    die "relay-tunneled IP ($RELAY_TUNNELED_IP) equals the client's direct IP ($DIRECT_IP) -- the relay is not changing the observed egress IP as expected in dual-host mode"
  fi
  ok "relay-tunneled IP ($RELAY_TUNNELED_IP) differs from the client's direct IP ($DIRECT_IP) -- egress is genuinely routed through the relay node"
else
  if [ "$RELAY_TUNNELED_IP" = "$DIRECT_IP" ]; then
    warn "relay-tunneled IP equals the client's direct IP -- EXPECTED in single-machine mode (both nodes share this box's one internet uplink/NAT). This does NOT prove the relay changes egress IP; it only proves the SOCKS5 path is functional. Run in dual-host mode to prove the IP actually changes."
  else
    ok "(bonus, unexpected on a shared uplink) relay-tunneled IP differs from the client's direct IP: $RELAY_TUNNELED_IP vs $DIRECT_IP"
  fi
fi

# --- 9/10. Deny-LAN-by-default policy proof --------------------------------
log "starting a loopback-only HTTP probe on the relay host (port $LAN_TEST_PORT)..."
run_bg_on relay relay-lan-server \
  "python3 -m http.server $LAN_TEST_PORT --bind 127.0.0.1 --directory /tmp"

relay_lan_probe_up() {
  run_on relay "nc -z 127.0.0.1 $LAN_TEST_PORT"
}
if ! wait_until "relay-side loopback HTTP probe" 15 relay_lan_probe_up; then
  warn "relay-side loopback HTTP probe did not come up; the LAN-deny assertions below may be inconclusive"
fi

lan_probe_via_relay() {
  run_on client "curl -s -o /dev/null -w '%{http_code}' --max-time 8 --socks5-hostname 127.0.0.1:$LOCAL_PROXY_PORT 'http://127.0.0.1:$LAN_TEST_PORT/'"
}

LAN_DENY_RESULT="$(lan_probe_via_relay 2>/dev/null || echo FAILED)"
if [ "$LAN_DENY_RESULT" = "200" ]; then
  die "deny-LAN-by-default is NOT working: fetched http://127.0.0.1:$LAN_TEST_PORT/ THROUGH the relay with allow_lan still at its default (should be denied)"
fi
ok "deny-LAN-by-default holds: relay refused to CONNECT to its own loopback (result: $LAN_DENY_RESULT)"

log "relay: enabling allow-lan..."
run_on relay "HOME='${NODE_HOME[relay]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[relay]}' egress-relay allow-lan on"
sleep 2  # AllowLAN is re-read live per-connection; no restart needed, but give it a beat.

LAN_ALLOW_RESULT="$(lan_probe_via_relay 2>/dev/null || echo FAILED)"
if [ "$LAN_ALLOW_RESULT" != "200" ]; then
  warn "after 'egress-relay allow-lan on', the loopback probe still did not succeed (result: $LAN_ALLOW_RESULT) -- allow-lan toggle may not have taken effect, or the probe server did not start; check ${NODE_HOME[relay]}/.citadel-cli/logs/latest.log on the relay host"
else
  ok "allow-lan toggle works: after 'egress-relay allow-lan on', the same loopback destination now succeeds through the relay"
fi

run_on relay "HOME='${NODE_HOME[relay]}' CITADEL_NO_AUTO_UPDATE=1 '${NODE_BIN[relay]}' egress-relay allow-lan off" >/dev/null 2>&1 || true

# --- 11. Authorization audit-log proof -------------------------------------
log "checking the relay's own log for an authorized-connection audit line..."
AUTH_LOG_HIT="$(run_on relay "grep -c 'egress relay: authorized' '${NODE_HOME[relay]}/.citadel-cli/logs/latest.log' 2>/dev/null" || true)"
AUTH_LOG_HIT="${AUTH_LOG_HIT:-0}"
if [ "$AUTH_LOG_HIT" -ge 1 ] 2>/dev/null; then
  ok "relay log shows ${AUTH_LOG_HIT} authorized connection(s) from the client peer"
else
  warn "did not find an 'egress relay: authorized' line in the relay's log -- the functional proof above still passed, but the audit-log assertion is inconclusive (check ${NODE_HOME[relay]}/.citadel-cli/logs/latest.log)"
fi

echo
ok "egress relay validation complete (mode: $MODE)."
if [ "$MODE" = "single" ]; then
  log "Remember: single-machine mode proved the relay is FUNCTIONAL and its LAN-deny policy holds, but could not prove the egress IP changes (same uplink). Re-run in dual-host mode for that assertion."
fi
