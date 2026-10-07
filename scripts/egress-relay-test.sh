#!/usr/bin/env bash
# End-to-end disposable-node validation for the Citadel mesh egress relay.
#
# This is intentionally separate from scripts/egress-relay-ci.sh. That script
# checks the standing dual-uplink lab; this one enrolls two clean, disposable
# nodes and validates login, same-owner authorization, relay-only serving,
# egress, deny-LAN, and the live allow-LAN toggle in one run.
set -Eeuo pipefail

NEXUS_URL="${NEXUS_URL:-https://nexus.aceteam.ai}"
AUTH_SERVICE="${AUTH_SERVICE:-}"
AUTHKEY_FILE="${CITADEL_TEST_AUTHKEY_FILE:-}"
AUTHKEY_CLIENT_FILE="${CITADEL_TEST_AUTHKEY_CLIENT_FILE:-${AUTHKEY_FILE}}"
API_KEY_FILE="${CITADEL_TEST_API_KEY_FILE:-}"
EXPECTED_ORG_ID="${CITADEL_TEST_EXPECT_ORG_ID:-}"
RELAY_HOST="${RELAY_HOST:-}"
CLIENT_HOST="${CLIENT_HOST:-}"
IP_ECHO_URL="${IP_ECHO_URL:-https://api.ipify.org}"
CITADEL_BIN_OVERRIDE="${CITADEL_BIN:-}"
NODE_PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"
DRY_RUN=0

RUN_ID="$(date +%s)-$$"
RELAY_NODE_NAME="egress-relay-test-relay-${RUN_ID}"
CLIENT_NODE_NAME="egress-relay-test-client-${RUN_ID}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BUILD_DIR=""
CLEANUP_DONE=0
CLEANUP_FAILED=0
LAN_RELAXED=0

declare -A NODE_HOST NODE_HOME NODE_BIN NODE_PID NODE_PID_ROLE NODE_PID_FINGERPRINT NODE_LOG LOGIN_CLEANUP_REQUIRED NODE_TERMINATION_UNCONFIRMED

log() { printf '%s\n' "[egress-relay-test] $*"; }
ok() { printf '%s\n' "[egress-relay-test] OK: $*"; }
warn() { printf '%s\n' "[egress-relay-test] WARN: $*" >&2; }
die() { printf '%s\n' "[egress-relay-test] FATAL: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: egress-relay-test.sh [--dry-run] [--relay-host USER@HOST --client-host USER@HOST]

Runs a disposable two-node egress-relay acceptance test. Dual-host mode is the
only mode which can prove that the public egress IP changes; the hosts must use
different public uplinks. With neither host option, two identities are run on
one never-enrolled machine for functional testing only.

Required for a real run:
  CITADEL_TEST_AUTHKEY_FILE          Local 0600/0400 regular file containing
                                     an ephemeral, reusable preauth key.
  CITADEL_TEST_API_KEY_FILE          Local 0600/0400 regular file containing
                                     an API key authorized to delete both nodes.

Optional:
  CITADEL_TEST_AUTHKEY_CLIENT_FILE   Separate same-org key file for the client.
  CITADEL_TEST_EXPECT_ORG_ID         Require whoami to report this org on both
                                     nodes (authkey-only nodes may not report it).
  NEXUS_URL                          Default: https://nexus.aceteam.ai
  AUTH_SERVICE                       Optional self-hosted auth-service URL.
  RELAY_HOST / CLIENT_HOST           SSH targets; both or neither are required.
  IP_ECHO_URL                        HTTPS public-IP endpoint.
  CITADEL_BIN                        Prebuilt binary; otherwise this checkout
                                     is built for the target OS/architecture.

Keys are streamed on stdin to login and strict logout; they are never
placed in an SSH command, child argv, process listing, or harness log. The old
CITADEL_TEST_AUTHKEY environment interface is deliberately rejected.
EOF
}

while (($#)); do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --relay-host) (($# >= 2)) || die "--relay-host requires a value"; RELAY_HOST="$2"; shift 2 ;;
    --client-host) (($# >= 2)) || die "--client-host requires a value"; CLIENT_HOST="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

if [[ -n "${CITADEL_TEST_AUTHKEY:-}" || -n "${CITADEL_TEST_AUTHKEY_CLIENT:-}" ]]; then
  die "refusing legacy authkey environment variables; store the key in a 0600 file and set CITADEL_TEST_AUTHKEY_FILE"
fi

if [[ -n "$RELAY_HOST" && -n "$CLIENT_HOST" ]]; then
  MODE=dual
  [[ "$RELAY_HOST" != "$CLIENT_HOST" ]] || die "relay and client SSH targets must be different hosts"
elif [[ -z "$RELAY_HOST" && -z "$CLIENT_HOST" ]]; then
  MODE=single
else
  die "set both RELAY_HOST and CLIENT_HOST, or neither"
fi

validate_ssh_target() {
  [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9_.:@%+-]*$ ]] && [[ "$1" != -* ]]
}
if [[ "$MODE" == dual ]]; then
  validate_ssh_target "$RELAY_HOST" || die "unsafe RELAY_HOST syntax"
  validate_ssh_target "$CLIENT_HOST" || die "unsafe CLIENT_HOST syntax"
fi
[[ "$NEXUS_URL" =~ ^https://[^[:space:]\']+$ ]] || die "NEXUS_URL must be an HTTPS URL without whitespace or quotes"
[[ -z "$AUTH_SERVICE" || "$AUTH_SERVICE" =~ ^https://[^[:space:]\']+$ ]] || die "AUTH_SERVICE must be an HTTPS URL without whitespace or quotes"
[[ "$IP_ECHO_URL" =~ ^https://[^[:space:]\']+$ ]] || die "IP_ECHO_URL must be an HTTPS URL without whitespace or quotes"

NODE_HOST[relay]="$RELAY_HOST"
NODE_HOST[client]="$CLIENT_HOST"

shell_quote() {
  local value="${1//\'/\'\\\'\'}"
  printf "'%s'" "$value"
}

remote_command() {
  local arg out=""
  for arg in "$@"; do
    [[ -z "$out" ]] || out+=" "
    out+="$(shell_quote "$arg")"
  done
  printf '%s' "$out"
}

run_on() {
  local who="$1"; shift
  local host="${NODE_HOST[$who]:-}"
  if [[ -n "$host" ]]; then
    ssh -o BatchMode=yes -o ConnectTimeout=10 -- "$host" "$(remote_command "$@")"
  else
    "$@"
  fi
}

node_cmd() {
  local who="$1"; shift
  # Start from an empty environment so ambient CITADEL_CA_CERT, node-dir,
  # proxy, API credential, or feature overrides cannot alter this disposable
  # identity or make a command partially mutate state under another scope.
  run_on "$who" env -i "HOME=${NODE_HOME[$who]}" "PATH=$NODE_PATH" CITADEL_NO_AUTO_UPDATE=1 \
    "CITADEL_AUTH_HOST=${AUTH_SERVICE:-https://aceteam.ai}" \
    "${NODE_BIN[$who]}" --no-color "$@"
}

node_cmd_stdin() {
  local who="$1" input="$2"; shift 2
  local host="${NODE_HOST[$who]:-}"
  local -a args=(env -i "HOME=${NODE_HOME[$who]}" "PATH=$NODE_PATH" CITADEL_NO_AUTO_UPDATE=1
    "CITADEL_AUTH_HOST=${AUTH_SERVICE:-https://aceteam.ai}"
    "${NODE_BIN[$who]}" --no-color "$@")
  if [[ -n "$host" ]]; then
    ssh -o BatchMode=yes -o ConnectTimeout=10 -- "$host" "$(remote_command "${args[@]}")" < "$input"
  else
    "${args[@]}" < "$input"
  fi
}

run_bg_on() {
  local who="$1" label="$2"
  shift 2
  local host out pid launch
  host="${NODE_HOST[$who]:-}"
  valid_harness_home "${NODE_HOME[$who]:-}" || die "cannot start $label without a guarded disposable home"
  # Keep logs below the 0700 mktemp home. Predictable files directly under
  # /tmp would permit another local user to pre-place a symlink and redirect
  # the harness's truncating open into a file owned by this account.
  out="${NODE_HOME[$who]}/.${label}.log"
  NODE_LOG[$label]="$out"
  if [[ -n "$host" ]]; then
    launch="nohup $(remote_command "$@") >$(shell_quote "$out") 2>&1 </dev/null & printf '%s\\n' \$!"
    pid="$(ssh -o BatchMode=yes -o ConnectTimeout=10 -- "$host" "$launch")"
  else
    "$@" >"$out" 2>&1 </dev/null &
    pid=$!
  fi
  [[ "$pid" =~ ^[0-9]+$ ]] || die "could not capture $label PID"
  NODE_PID[$label]="$pid"
  NODE_PID_ROLE[$label]="$who"
  local fingerprint
  fingerprint="$(run_on "$who" ps -p "$pid" -o lstart= -o args=)" \
    || die "$label exited before its process identity could be captured"
  [[ -n "$fingerprint" ]] || die "could not fingerprint $label PID"
  NODE_PID_FINGERPRINT[$label]="$fingerprint"
}

inspect_bg() {
  local who="$1" pid="$2" output status
  if output="$(run_on "$who" sh -c '
    output=$(ps -p "$1" -o lstart= -o args= 2>/dev/null)
    status=$?
    case "$status" in
      0) [ -n "$output" ] || exit 70; printf "%s" "$output" ;;
      1) exit 3 ;;
      *) exit 70 ;;
    esac
  ' sh "$pid")"; then
    printf '%s' "$output"
    return 0
  else
    status=$?
  fi
  # The remote helper reserves 3 for a clean ps "no such process" result.
  # SSH transport failures and every other ps/protocol failure are unknown,
  # never evidence that it is safe to deregister or delete the guarded HOME.
  [[ "$status" == 3 ]] && return 1
  return 2
}

wait_bg_gone() {
  local who="$1" pid="$2" expected="$3" attempts=0 current status
  while ((attempts < 20)); do
    if current="$(inspect_bg "$who" "$pid" 2>/dev/null)"; then
      [[ "$current" == "$expected" ]] || return 3
    else
      status=$?
      [[ "$status" == 1 ]] && return 0
      return 2
    fi
    sleep 0.1
    ((attempts += 1))
  done
  return 1
}

retain_unconfirmed_bg() {
  local who="$1" label="$2" pid="$3" reason="$4"
  warn "$reason for $label PID $pid; retaining PID/fingerprint, registration, logs, and guarded HOME for recovery"
  CLEANUP_FAILED=1
  NODE_TERMINATION_UNCONFIRMED[$who]=1
}

kill_bg() {
  local label="$1" pid who expected current status
  pid="${NODE_PID[$label]:-}"
  who="${NODE_PID_ROLE[$label]:-}"
  [[ -n "$pid" && -n "$who" ]] || return 0
  expected="${NODE_PID_FINGERPRINT[$label]:-}"
  if current="$(inspect_bg "$who" "$pid" 2>/dev/null)"; then
    :
  else
    status=$?
    if [[ "$status" == 1 ]]; then
      unset 'NODE_PID[$label]' 'NODE_PID_FINGERPRINT[$label]'
      return 0
    fi
    retain_unconfirmed_bg "$who" "$label" "$pid" "could not inspect process identity"
    return 1
  fi
  if [[ -z "$expected" || "$current" != "$expected" ]]; then
    retain_unconfirmed_bg "$who" "$label" "$pid" "refusing to signal reused or unverified process"
    return 1
  fi
  if ! run_on "$who" kill -TERM "$pid" >/dev/null 2>&1; then
    retain_unconfirmed_bg "$who" "$label" "$pid" "TERM delivery failed"
    return 1
  fi
  if wait_bg_gone "$who" "$pid" "$expected"; then
    unset 'NODE_PID[$label]' 'NODE_PID_FINGERPRINT[$label]'
    return 0
  else
    status=$?
  fi
  case "$status" in
    2) retain_unconfirmed_bg "$who" "$label" "$pid" "post-TERM process inspection failed"; return 1 ;;
    3) retain_unconfirmed_bg "$who" "$label" "$pid" "refusing KILL for a reused process"; return 1 ;;
  esac
  # Re-check immediately before the irreversible KILL escalation. The last
  # wait-loop probe may precede this point by a sleep interval, during which
  # the original process could exit and its numeric PID could be reused.
  if current="$(inspect_bg "$who" "$pid" 2>/dev/null)"; then
    if [[ "$current" != "$expected" ]]; then
      retain_unconfirmed_bg "$who" "$label" "$pid" "refusing KILL for a reused process"
      return 1
    fi
  else
    status=$?
    if [[ "$status" == 1 ]]; then
      unset 'NODE_PID[$label]' 'NODE_PID_FINGERPRINT[$label]'
      return 0
    fi
    retain_unconfirmed_bg "$who" "$label" "$pid" "pre-KILL process inspection failed"
    return 1
  fi
  if ! run_on "$who" kill -KILL "$pid" >/dev/null 2>&1; then
    retain_unconfirmed_bg "$who" "$label" "$pid" "KILL delivery failed"
    return 1
  fi
  if wait_bg_gone "$who" "$pid" "$expected"; then
    unset 'NODE_PID[$label]' 'NODE_PID_FINGERPRINT[$label]'
    return 0
  else
    status=$?
  fi
  case "$status" in
    1) retain_unconfirmed_bg "$who" "$label" "$pid" "process remained live after KILL" ;;
    2) retain_unconfirmed_bg "$who" "$label" "$pid" "post-KILL process inspection failed" ;;
    3) retain_unconfirmed_bg "$who" "$label" "$pid" "PID changed after KILL" ;;
  esac
  return 1
}

valid_harness_home() {
  [[ "$1" =~ ^/tmp/egress-relay-test-(relay|client)\.[A-Za-z0-9]+$ ]]
}

cleanup() {
  ((CLEANUP_DONE == 0)) || return 0
  CLEANUP_DONE=1
  log "cleaning up disposable nodes"

  if ((LAN_RELAXED == 1)) && [[ -n "${NODE_BIN[relay]:-}" ]]; then
    node_cmd relay egress-relay allow-lan off >/dev/null 2>&1 || { warn "could not restore deny-LAN config"; CLEANUP_FAILED=1; }
    LAN_RELAXED=0
  fi
  kill_bg client-proxy || true
  kill_bg relay-probe || true
  kill_bg relay-serve || true

  local who home marker deregistered
  for who in client relay; do
    if [[ "${NODE_TERMINATION_UNCONFIRMED[$who]:-0}" == 1 ]]; then
      warn "$who process termination is unconfirmed; retaining registration and guarded home for recovery"
      CLEANUP_FAILED=1
      continue
    fi
    deregistered=1
    if [[ "${LOGIN_CLEANUP_REQUIRED[$who]:-0}" == 1 && -n "${NODE_BIN[$who]:-}" ]]; then
      if ! node_cmd_stdin "$who" "$API_KEY_FILE" logout --force --api-key-stdin --require-deregister >/dev/null 2>&1; then
        warn "$who strict deregistration/logout failed; retaining its guarded home for retry"
        CLEANUP_FAILED=1
        deregistered=0
      fi
    fi
    home="${NODE_HOME[$who]:-}"
    if [[ -n "$home" ]]; then
      if ! valid_harness_home "$home"; then
        warn "refusing cleanup of unexpected $who path: $home"
        CLEANUP_FAILED=1
        continue
      fi
      if ((deregistered == 0)); then
        continue
      fi
      marker="${RUN_ID}:${who}"
      run_on "$who" sh -c 'test "$(cat "$1/.citadel-egress-harness" 2>/dev/null)" = "$2" && rm -rf -- "$1"' sh "$home" "$marker" \
        || { warn "marker-guarded cleanup failed for $who"; CLEANUP_FAILED=1; }
    fi
  done

  local label
  for label in "${!NODE_LOG[@]}"; do
    who="${NODE_PID_ROLE[$label]:-}"
    if [[ -n "$who" ]]; then
      [[ "${NODE_TERMINATION_UNCONFIRMED[$who]:-0}" == 1 ]] && continue
      run_on "$who" rm -f -- "${NODE_LOG[$label]}" >/dev/null 2>&1 || true
    fi
  done
  if [[ -n "$BUILD_DIR" ]]; then
    if [[ "$BUILD_DIR" =~ ^/tmp/egress-relay-test-build\.[A-Za-z0-9]+$ ]]; then
      rm -rf -- "$BUILD_DIR"
    else
      warn "refusing cleanup of unexpected build path: $BUILD_DIR"
      CLEANUP_FAILED=1
    fi
  fi
  ((CLEANUP_FAILED == 0))
}

on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  cleanup || { ((rc != 0)) || rc=1; }
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

preflight_host() {
  local who="$1"
  run_on "$who" sh -c '
    set -eu
    if [ "$(id -u)" -eq 0 ]; then echo "refusing root: HOME isolation is not trustworthy" >&2; exit 42; fi
    if [ -n "${SUDO_USER:-}" ]; then echo "refusing inherited SUDO_USER" >&2; exit 42; fi
    case "$(uname -s)" in Darwin) g=/usr/local/etc/citadel ;; *) g=/etc/citadel ;; esac
    if [ -e "$g/config.yaml" ] || [ -e "$g/state-dir" ]; then echo "machine-global Citadel state exists" >&2; exit 42; fi
    if [ -d "$HOME/citadel-node/network" ] && [ -n "$(find "$HOME/citadel-node/network" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]; then
      echo "existing Citadel network state exists in the account home" >&2; exit 42
    fi
    for c in bash curl python3 mktemp ps uname; do command -v "$c" >/dev/null || { echo "missing prerequisite: $c" >&2; exit 43; }; done
  '
}

validate_secret_file() {
  local label="$1" path="$2" mode size
  [[ -n "$path" ]] || die "$label is required"
  [[ -f "$path" && ! -L "$path" && -O "$path" ]] || die "$label must name a regular, non-symlink file owned by this user"
  case "$(uname -s)" in
    Darwin) mode="$(stat -f '%Lp' "$path")" ;;
    *) mode="$(stat -c '%a' "$path")" ;;
  esac
  [[ "$mode" == 600 || "$mode" == 400 ]] || die "$label must be mode 0600 or 0400 (mode $mode)"
  size="$(wc -c < "$path")"
  ((size > 0 && size <= 4096)) || die "$label must contain 1..4096 bytes"
}

make_home() {
  local who="$1" prefix home marker
  prefix="/tmp/egress-relay-test-${who}."
  marker="${RUN_ID}:${who}"
  home="$(run_on "$who" mktemp -d "${prefix}XXXXXXXX")"
  valid_harness_home "$home" || die "mktemp returned unsafe $who path"
  NODE_HOME[$who]="$home"
  run_on "$who" sh -c 'umask 077; printf "%s\n" "$2" > "$1/.citadel-egress-harness"' sh "$home" "$marker"
}

target_tuple() {
  local who="$1" os arch
  os="$(run_on "$who" uname -s)"
  arch="$(run_on "$who" uname -m)"
  case "$os" in Linux) os=linux ;; Darwin) os=darwin ;; *) die "unsupported target OS: $os" ;; esac
  case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported target architecture: $arch" ;; esac
  printf '%s/%s' "$os" "$arch"
}

build_binary() {
  local relay_tuple client_tuple goos goarch
  relay_tuple="$(target_tuple relay)"
  client_tuple="$(target_tuple client)"
  [[ "$relay_tuple" == "$client_tuple" ]] || die "relay ($relay_tuple) and client ($client_tuple) need separate binaries; use matching hosts"
  if [[ -n "$CITADEL_BIN_OVERRIDE" ]]; then
    [[ -x "$CITADEL_BIN_OVERRIDE" ]] || die "CITADEL_BIN is not executable"
    BIN="$CITADEL_BIN_OVERRIDE"
    return
  fi
  BUILD_DIR="$(mktemp -d /tmp/egress-relay-test-build.XXXXXXXX)"
  goos="${relay_tuple%/*}"; goarch="${relay_tuple#*/}"
  log "building Citadel for $relay_tuple" >&2
  (cd "$REPO_ROOT" && env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -o "$BUILD_DIR/citadel" ./cmd/citadel) >&2
  BIN="$BUILD_DIR/citadel"
}

install_binary() {
  local who="$1" source="$2" host
  host="${NODE_HOST[$who]:-}"
  if [[ -n "$host" ]]; then
    scp -q -- "$source" "${host}:${NODE_HOME[$who]}/citadel"
    NODE_BIN[$who]="${NODE_HOME[$who]}/citadel"
    run_on "$who" chmod 0700 "${NODE_BIN[$who]}"
  else
    NODE_BIN[$who]="$source"
  fi
}

login_on() {
  local who="$1" keyfile="$2" node_name="$3" host
  host="${NODE_HOST[$who]:-}"
  local -a args=(env -i "HOME=${NODE_HOME[$who]}" "PATH=$NODE_PATH" CITADEL_NO_AUTO_UPDATE=1
    "CITADEL_AUTH_HOST=${AUTH_SERVICE:-https://aceteam.ai}"
    "${NODE_BIN[$who]}" --no-color --nexus "$NEXUS_URL")
  [[ -z "$AUTH_SERVICE" ]] || args+=(--auth-service "$AUTH_SERVICE")
  args+=(login --authkey-stdin --node-name "$node_name")
  log "$who: enrolling $node_name (authkey streamed on stdin; value not logged)"
  # Login can create mesh state before a later CA/config persistence step
  # fails. Arm cleanup before starting it, not only after a zero exit status.
  LOGIN_CLEANUP_REQUIRED[$who]=1
  if [[ -n "$host" ]]; then
    ssh -o BatchMode=yes -o ConnectTimeout=10 -- "$host" "$(remote_command "${args[@]}")" < "$keyfile"
  else
    "${args[@]}" < "$keyfile"
  fi
}

json_field() {
  local field="$1"
  python3 -c 'import json,sys; v=json.load(sys.stdin).get(sys.argv[1], ""); print(str(v).lower() if isinstance(v,bool) else v)' "$field"
}

identity_json() {
  node_cmd "$1" whoami --json
}

assert_ip() {
  python3 -c 'import ipaddress,sys; ipaddress.ip_address(sys.argv[1])' "$1" >/dev/null 2>&1
}

choose_port() {
  run_on "$1" python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

wait_until() {
  local desc="$1" timeout="$2"; shift 2
  local end=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < end)) || { warn "timed out waiting for $desc"; return 1; }
    sleep 1
  done
}

port_open() {
  local who="$1" port="$2"
  run_on "$who" bash -c ': </dev/tcp/127.0.0.1/$1' bash "$port"
}

validate_public_ip_response() {
  python3 -c '
import ipaddress
import sys

raw = sys.stdin.buffer.read(4097)
if len(raw) > 4096:
    raise SystemExit("public IP response exceeds 4096 bytes")
try:
    value = raw.decode("ascii").strip()
except UnicodeDecodeError:
    raise SystemExit("public IP response is not ASCII")
if not value or any(ch.isspace() for ch in value) or "%" in value:
    raise SystemExit("public IP response must contain exactly one bare address")
try:
    address = ipaddress.ip_address(value)
except ValueError:
    raise SystemExit("public IP response is not one IP address")
if (not address.is_global or address.is_loopback or address.is_private or
        address.is_link_local or address.is_multicast or address.is_reserved or
        address.is_unspecified or getattr(address, "is_site_local", False)):
    raise SystemExit("public IP response is not globally routable")
print(address.compressed)
'
}

fetch_ip() {
  local who="$1"; shift
  if (($# == 0)); then
    # Prove each host's direct route. Ignore ambient HTTP(S)/ALL_PROXY both by
    # emptying the environment and by explicitly bypassing proxy discovery.
    run_on "$who" env -i "PATH=$NODE_PATH" curl --noproxy '*' --fail --silent --show-error --max-time 20 "$IP_ECHO_URL"
  else
    # The explicit SOCKS route is the behavior under test. Emptying the
    # environment prevents ambient proxy variables from replacing it; do not
    # use --noproxy here because that would bypass this explicit tunnel too.
    run_on "$who" env -i "PATH=$NODE_PATH" curl --fail --silent --show-error --max-time 20 "$@" "$IP_ECHO_URL"
  fi | validate_public_ip_response
}

log "mode: $MODE"
if ((DRY_RUN == 1)); then
  cat <<EOF
[egress-relay-test] dry-run only; no SSH, enrollment, process, or file mutation performed
  relay:  ${RELAY_HOST:-local disposable identity} ($RELAY_NODE_NAME)
  client: ${CLIENT_HOST:-local disposable identity} ($CLIENT_NODE_NAME)
  auth:   credentials streamed from 0600 files; strict deregistration required
  relay:  citadel egress-relay serve (no worker or Redis dependency)
  gates:  exact node identities; optional org ID; distinct mesh IDs/IPs;
          relayed egress equals relay egress; dual-host egress changes;
          deny-LAN -> allow-LAN -> deny-LAN; exact client-IP auth log
EOF
  trap - EXIT INT TERM
  exit 0
fi

validate_secret_file CITADEL_TEST_AUTHKEY_FILE "$AUTHKEY_FILE"
validate_secret_file CITADEL_TEST_AUTHKEY_CLIENT_FILE "$AUTHKEY_CLIENT_FILE"
validate_secret_file CITADEL_TEST_API_KEY_FILE "$API_KEY_FILE"
preflight_host relay || die "relay host is not a clean disposable Citadel target"
preflight_host client || die "client host is not a clean disposable Citadel target"

make_home relay
make_home client
BIN=""
build_binary
install_binary relay "$BIN"
install_binary client "$BIN"

login_on relay "$AUTHKEY_FILE" "$RELAY_NODE_NAME"
login_on client "$AUTHKEY_CLIENT_FILE" "$CLIENT_NODE_NAME"

RELAY_ID="$(identity_json relay)"
CLIENT_ID="$(identity_json client)"
RELAY_NAME="$(printf '%s' "$RELAY_ID" | json_field node_name)"
CLIENT_NAME="$(printf '%s' "$CLIENT_ID" | json_field node_name)"
RELAY_MESH_IP="$(printf '%s' "$RELAY_ID" | json_field mesh_ipv4)"
CLIENT_MESH_IP="$(printf '%s' "$CLIENT_ID" | json_field mesh_ipv4)"
RELAY_MESH_ID="$(printf '%s' "$RELAY_ID" | json_field headscale_node_id)"
CLIENT_MESH_ID="$(printf '%s' "$CLIENT_ID" | json_field headscale_node_id)"
RELAY_ORG="$(printf '%s' "$RELAY_ID" | json_field org_id)"
CLIENT_ORG="$(printf '%s' "$CLIENT_ID" | json_field org_id)"

[[ "$RELAY_NAME" == "$RELAY_NODE_NAME" ]] || die "relay identity mismatch: expected $RELAY_NODE_NAME, got ${RELAY_NAME:-<empty>}"
[[ "$CLIENT_NAME" == "$CLIENT_NODE_NAME" ]] || die "client identity mismatch: expected $CLIENT_NODE_NAME, got ${CLIENT_NAME:-<empty>}"
assert_ip "$RELAY_MESH_IP" || die "relay did not report a valid mesh IP"
assert_ip "$CLIENT_MESH_IP" || die "client did not report a valid mesh IP"
[[ "$RELAY_MESH_IP" != "$CLIENT_MESH_IP" ]] || die "relay and client resolved to the same mesh IP"
[[ -n "$RELAY_MESH_ID" && -n "$CLIENT_MESH_ID" && "$RELAY_MESH_ID" != "$CLIENT_MESH_ID" ]] || die "relay and client need distinct Headscale node IDs"
if [[ -n "$EXPECTED_ORG_ID" ]]; then
  [[ "$RELAY_ORG" == "$EXPECTED_ORG_ID" && "$CLIENT_ORG" == "$EXPECTED_ORG_ID" ]] || die "nodes did not both report expected org $EXPECTED_ORG_ID"
elif [[ -n "$RELAY_ORG" || -n "$CLIENT_ORG" ]]; then
  [[ -n "$RELAY_ORG" && "$RELAY_ORG" == "$CLIENT_ORG" ]] || die "reported node orgs do not match"
else
  log "authkey enrollment did not expose org IDs; same-owner scope will be proven by the exact client-IP authorization log"
fi
ok "distinct expected nodes enrolled: relay=$RELAY_MESH_IP client=$CLIENT_MESH_IP"

# Establish a known fail-closed baseline before the listener starts. `serve`
# force-enables the relay and has no Redis/worker dependency (#1037).
node_cmd relay egress-relay allow-lan off >/dev/null
run_bg_on relay relay-serve env -i "HOME=${NODE_HOME[relay]}" "PATH=$NODE_PATH" CITADEL_NO_AUTO_UPDATE=1 \
  "CITADEL_AUTH_HOST=${AUTH_SERVICE:-https://aceteam.ai}" \
  "${NODE_BIN[relay]}" --no-color egress-relay serve

LOCAL_PROXY_PORT="$(choose_port client)"
LAN_TEST_PORT="$(choose_port relay)"
run_bg_on client client-proxy env -i "HOME=${NODE_HOME[client]}" "PATH=$NODE_PATH" CITADEL_NO_AUTO_UPDATE=1 \
  "CITADEL_AUTH_HOST=${AUTH_SERVICE:-https://aceteam.ai}" \
  "${NODE_BIN[client]}" --no-color proxy "$LOCAL_PROXY_PORT" "${RELAY_MESH_IP}:7861" --bind 127.0.0.1
wait_until "client proxy port" 45 port_open client "$LOCAL_PROXY_PORT" || die "client proxy did not start"

DIRECT_IP="$(fetch_ip client)" || die "client direct egress lookup failed"
RELAY_OWN_IP="$(fetch_ip relay)" || die "relay direct egress lookup failed"
RELAY_TUNNELED_IP="$(fetch_ip client --socks5-hostname "127.0.0.1:${LOCAL_PROXY_PORT}")" || die "relayed egress lookup failed"
assert_ip "$DIRECT_IP" || die "client direct endpoint returned a non-IP response"
assert_ip "$RELAY_OWN_IP" || die "relay direct endpoint returned a non-IP response"
assert_ip "$RELAY_TUNNELED_IP" || die "relay endpoint returned a non-IP response"
[[ "$RELAY_TUNNELED_IP" == "$RELAY_OWN_IP" ]] || die "relayed egress $RELAY_TUNNELED_IP does not equal relay egress $RELAY_OWN_IP"
if [[ "$MODE" == dual ]]; then
  [[ "$RELAY_TUNNELED_IP" != "$DIRECT_IP" ]] || die "dual-host relay did not change public egress IP ($DIRECT_IP)"
  ok "egress changed from client $DIRECT_IP to relay $RELAY_TUNNELED_IP"
else
  warn "single-machine mode cannot prove an egress-IP change; both identities share one uplink"
fi

PROBE_TOKEN="egress-relay-probe-${RUN_ID}"
run_on relay sh -c 'umask 077; printf "%s\n" "$2" > "$1/probe"' sh "${NODE_HOME[relay]}" "$PROBE_TOKEN"
run_bg_on relay relay-probe python3 -m http.server "$LAN_TEST_PORT" --bind 127.0.0.1 --directory "${NODE_HOME[relay]}"
wait_until "relay loopback probe" 15 port_open relay "$LAN_TEST_PORT" || die "relay loopback probe did not start"

probe_via_relay() {
  run_on client env -i "PATH=$NODE_PATH" curl --fail --silent --show-error --max-time 8 \
    --socks5-hostname "127.0.0.1:${LOCAL_PROXY_PORT}" "http://127.0.0.1:${LAN_TEST_PORT}/probe"
}

RELAY_APP_LOG="${NODE_HOME[relay]}/.citadel-cli/logs/latest.log"
policy_denial_count() {
  (run_on relay cat "$RELAY_APP_LOG" 2>/dev/null || true) | python3 -c '
import sys
client, target = sys.argv[1:]
print(sum(
    client in line
    and target in line
    and "loopback destination denied (allow_lan is off)" in line
    for line in sys.stdin
))
' "socks: ${CLIENT_MESH_IP}:" "dial 127.0.0.1:${LAN_TEST_PORT}"
}

policy_denial_logged_after() {
  local before="$1" after
  after="$(policy_denial_count)"
  [[ "$after" =~ ^[0-9]+$ ]] && ((after > before))
}

assert_tunnel_healthy() {
  local after="$1" tunneled
  tunneled="$(fetch_ip client --socks5-hostname "127.0.0.1:${LOCAL_PROXY_PORT}")" \
    || die "public relay tunnel failed after $after"
  assert_ip "$tunneled" || die "public relay tunnel returned a non-IP after $after"
  [[ "$tunneled" == "$RELAY_OWN_IP" ]] \
    || die "public relay tunnel used $tunneled instead of relay egress $RELAY_OWN_IP after $after"
}

DENIALS_BEFORE="$(policy_denial_count)"
if probe_via_relay >/dev/null 2>&1; then
  die "deny-LAN default failed: client reached relay loopback"
fi
wait_until "policy-specific initial loopback denial log" 5 policy_denial_logged_after "$DENIALS_BEFORE" \
  || die "initial curl failure was not accompanied by the relay's loopback-policy denial"
assert_tunnel_healthy "the initial LAN refusal"
ok "deny-LAN blocked relay loopback"

node_cmd relay egress-relay allow-lan on >/dev/null
LAN_RELAXED=1
ALLOW_DEADLINE=$((SECONDS + 15))
while [[ "$(probe_via_relay 2>/dev/null || true)" != "$PROBE_TOKEN" ]]; do
  ((SECONDS < ALLOW_DEADLINE)) || die "allow-LAN did not permit the same loopback destination"
  sleep 1
done
ok "allow-LAN permitted the same loopback destination without restarting the relay"

node_cmd relay egress-relay allow-lan off >/dev/null
LAN_RELAXED=0
DENIALS_BEFORE="$(policy_denial_count)"
if probe_via_relay >/dev/null 2>&1; then
  die "deny-LAN was not restored after allow-lan off"
fi
wait_until "policy-specific restored loopback denial log" 5 policy_denial_logged_after "$DENIALS_BEFORE" \
  || die "restored curl failure was not accompanied by the relay's loopback-policy denial"
assert_tunnel_healthy "the restored LAN refusal"
ok "deny-LAN was restored"

AUTH_PATTERN="egress relay: authorized ${CLIENT_MESH_IP}:"
AUTH_HITS="$(run_on relay grep -F -c -- "$AUTH_PATTERN" "$RELAY_APP_LOG" 2>/dev/null || true)"
[[ "${AUTH_HITS:-0}" =~ ^[0-9]+$ ]] && ((AUTH_HITS >= 1)) \
  || die "relay log lacks authorization for exact client mesh IP $CLIENT_MESH_IP"
ok "relay authorized the exact client mesh identity ($CLIENT_MESH_IP); same-owner gate passed"

ok "disposable egress-relay acceptance passed; cleanup will now logout and remove local state"
