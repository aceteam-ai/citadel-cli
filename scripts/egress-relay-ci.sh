#!/usr/bin/env bash
#
# scripts/egress-relay-ci.sh
#
# Live cross-public-IP canary for the on-node SOCKS5 egress relay
# (citadel #787 / PR #980, relay-only serve mode #1037/#1033).
#
# WHAT IT PROVES (and why it needs TWO public IPs):
#   A "client" citadel node on one internet uplink tunnels an outbound HTTPS
#   request THROUGH a "relay" citadel node on a DIFFERENT internet uplink, and
#   the request is observed by the internet as coming from the RELAY's public
#   IP, not the client's. That "the egress IP actually changed" assertion is
#   physically impossible to make on a normal CI runner (one uplink, one NAT) --
#   it is only meaningful on a fabric with two independent public egresses.
#   This is the automated form of Act 4 of docs/demo-sovereign-fabric-runbook.md.
#
# TOPOLOGY (the aceteam dual-uplink pve host, 192.168.2.4):
#   - RELAY  = CT114 "citadel-relay"  on Cogeco cable, runs `citadel egress-relay
#              serve` (systemd unit citadel-relay.service), mesh SOCKS5 :7861.
#   - CLIENT = CT113 "citadel-client" on Bell fiber, runs `citadel proxy 1055
#              100.64.0.53:7861 --bind 127.0.0.1` (systemd unit
#              citadel-proxy.service), exposing a LOCAL SOCKS5 at 127.0.0.1:1055
#              that citadel's own userspace mesh dial bridges to the relay. No
#              kernel TUN is needed on the client for this to work.
#   The self-hosted GitHub Actions runner lives INSIDE CT113, so "direct" here
#   is the Bell uplink and "relayed" (via 127.0.0.1:1055) is the Cogeco uplink.
#
# ASSERTIONS (in order; each has a DISTINCT exit code so the failure is legible):
#   [infra=2] the client can reach the internet at all (direct fetch succeeds)
#   [relay=3] the relay path is reachable (relayed fetch succeeds) -- i.e. both
#             citadel-relay (CT114) and citadel-proxy (CT113) are actually up
#   [flip=1]  the relayed public IP DIFFERS from the direct public IP -- the
#             sovereign-egress proof. Drift-proof: it never hardcodes an ISP IP.
#   [1]       (optional) if EXPECT_RELAY_IP / EXPECT_CLIENT_IP are set, the
#             observed IPs must match them exactly. Left unset by default so a
#             residential IP change never causes a false red.
#
# This script is intentionally dependency-light (curl + POSIX-ish bash only; no
# jq, no go toolchain) because the client CT is a 1-vCPU / 1-GB box.
#
set -uo pipefail

# --------------------------------------------------------------------------
# Configuration (all overridable via env; the workflow wires these from repo
# variables/inputs).
# --------------------------------------------------------------------------
PROXY_ENDPOINT="${PROXY_ENDPOINT:-127.0.0.1:1055}"       # local SOCKS5 -> relay mesh :7861
IP_ECHO_PRIMARY="${IP_ECHO_PRIMARY:-https://api.ipify.org}"
IP_ECHO_SECONDARY="${IP_ECHO_SECONDARY:-https://ifconfig.me/ip}"
EXPECT_RELAY_IP="${EXPECT_RELAY_IP:-}"                    # optional hard check
EXPECT_CLIENT_IP="${EXPECT_CLIENT_IP:-}"                 # optional hard check
CURL_MAX_TIME="${CURL_MAX_TIME:-25}"
RETRIES="${RETRIES:-3}"
RETRY_SLEEP="${RETRY_SLEEP:-3}"

# --------------------------------------------------------------------------
# Small utilities
# --------------------------------------------------------------------------
c_red='\033[0;31m'; c_green='\033[0;32m'; c_yellow='\033[0;33m'; c_reset='\033[0m'
log()  { printf '%b\n' "[egress-ci] $*"; }
ok()   { printf '%b\n' "${c_green}[egress-ci] OK: $*${c_reset}"; }
warn() { printf '%b\n' "${c_yellow}[egress-ci] WARN: $*${c_reset}" >&2; }
err()  { printf '%b\n' "${c_red}[egress-ci] FAIL: $*${c_reset}" >&2; }

# Accepts a dotted IPv4 or a (loosely-validated) IPv6 literal. We do not need
# strict RFC parsing here -- just enough to reject an HTML error page or empty
# body from a flaky ip-echo provider.
valid_ip() {
  local ip="$1"
  case "$ip" in
    *[!0-9.]*)
      # not pure-IPv4 charset; accept only if it looks like IPv6 (has a colon
      # and only hex/colon chars)
      case "$ip" in
        *:*) case "$ip" in *[!0-9a-fA-F:]*) return 1;; *) return 0;; esac ;;
        *) return 1 ;;
      esac
      ;;
    *.*.*.*) return 0 ;;   # four dotted groups of digits
    *) return 1 ;;
  esac
}

# fetch_ip <label> <curl-extra-args...> -- tries primary then secondary echo
# service, with retries, and echoes the first valid IP to stdout. Returns
# non-zero if no provider yielded a valid IP.
fetch_ip() {
  local label="$1"; shift
  local url ip attempt
  for url in "$IP_ECHO_PRIMARY" "$IP_ECHO_SECONDARY"; do
    [ -n "$url" ] || continue
    attempt=1
    while [ "$attempt" -le "$RETRIES" ]; do
      ip="$(curl -fsS --max-time "$CURL_MAX_TIME" "$@" "$url" 2>/dev/null | tr -d '[:space:]')"
      if [ -n "$ip" ] && valid_ip "$ip"; then
        printf '%s' "$ip"
        return 0
      fi
      warn "$label: attempt $attempt via ${url} yielded '${ip:-<empty>}'"
      attempt=$((attempt + 1))
      sleep "$RETRY_SLEEP"
    done
  done
  return 1
}

summary() {
  # Append a markdown row/line to the GitHub step summary when running in CI.
  [ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
  printf '%b\n' "$*" >>"$GITHUB_STEP_SUMMARY"
}

# --------------------------------------------------------------------------
# Run
# --------------------------------------------------------------------------
log "proxy endpoint : socks5://${PROXY_ENDPOINT}"
log "ip echo         : ${IP_ECHO_PRIMARY} (fallback ${IP_ECHO_SECONDARY:-none})"

summary "## Dual-public-IP egress canary"
summary ""
summary "| check | value |"
summary "|---|---|"

# [infra] direct egress (the runner's own uplink, Bell)
direct="$(fetch_ip 'direct')" || {
  err "could not reach the internet directly from the client -- runner/network is broken (not a relay problem)."
  summary "| direct (client uplink) | ❌ unreachable |"
  exit 2
}
log "direct  (client uplink): ${direct}"
summary "| direct (client uplink) | \`${direct}\` |"

# [relay] egress through the relay (Cogeco)
relayed="$(fetch_ip 'relayed' --socks5-hostname "$PROXY_ENDPOINT")" || {
  err "the relay path is unreachable. Is 'citadel egress-relay serve' (CT114 citadel-relay.service) up, and 'citadel proxy ... ${PROXY_ENDPOINT}' (CT113 citadel-proxy.service) up? See docs/ci-dual-ip-egress-runner.md."
  summary "| relayed (via relay) | ❌ relay path down |"
  exit 3
}
log "relayed (via relay)   : ${relayed}"
summary "| relayed (via relay) | \`${relayed}\` |"

# [flip] the core sovereign-egress proof
if [ "$direct" = "$relayed" ]; then
  err "egress did NOT change: direct and relayed both egress from ${direct}. The tunnel is not routing through the relay's uplink (misrouted, or client and relay share an uplink)."
  summary "| **egress changed** | ❌ same IP (${direct}) |"
  exit 1
fi
ok "egress changed across two public IPs: ${direct} (client) -> ${relayed} (relay). Sovereign egress verified."
summary "| **egress changed** | ✅ \`${direct}\` → \`${relayed}\` |"

# [optional hard checks] only when explicitly configured
rc=0
if [ -n "$EXPECT_CLIENT_IP" ] && [ "$direct" != "$EXPECT_CLIENT_IP" ]; then
  err "direct egress ${direct} != EXPECT_CLIENT_IP ${EXPECT_CLIENT_IP} (client ISP IP may have changed; update the repo variable)."
  summary "| expect client IP | ❌ got ${direct}, want ${EXPECT_CLIENT_IP} |"
  rc=1
fi
if [ -n "$EXPECT_RELAY_IP" ] && [ "$relayed" != "$EXPECT_RELAY_IP" ]; then
  err "relayed egress ${relayed} != EXPECT_RELAY_IP ${EXPECT_RELAY_IP} (relay ISP IP may have changed; update the repo variable)."
  summary "| expect relay IP | ❌ got ${relayed}, want ${EXPECT_RELAY_IP} |"
  rc=1
fi
[ "$rc" -eq 0 ] || exit 1

summary ""
summary "_The relayed request left the fabric from a different public IP than the client's own uplink — provable only because this fabric has two independent egresses._"
ok "all egress canary assertions passed."
exit 0
