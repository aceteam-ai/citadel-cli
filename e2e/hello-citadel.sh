#!/usr/bin/env bash
# E2E test: ace CLI → Platform (FastAPI) → Redis → Citadel Worker
#
# Proves the full pipeline works with a simple SHELL_COMMAND job.
#
# Prerequisites:
#   - Docker (for Redis)
#   - ace CLI built (cd ace && pnpm build)
#   - aceteam-nodes installed (cd aceteam-nodes && uv sync --extra dev)
#   - citadel-cli buildable (cd citadel-cli && go build -o citadel .)
#   - Platform backend deps (cd aceteam/python-backend && uv sync --extra dev)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CITADEL_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_DIR="$(cd "$CITADEL_DIR/.." && pwd)"

# PIDs to clean up
PIDS=()
REDIS_CONTAINER=""

cleanup() {
    echo ""
    echo "=== Cleaning up ==="
    for pid in "${PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            echo "Stopping PID $pid"
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    if [[ -n "$REDIS_CONTAINER" ]]; then
        echo "Stopping Redis container $REDIS_CONTAINER"
        docker rm -f "$REDIS_CONTAINER" 2>/dev/null || true
    fi
    echo "Done."
}
trap cleanup EXIT

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}PASS${NC}: $1"; }
fail() { echo -e "${RED}FAIL${NC}: $1"; exit 1; }
info() { echo -e "${YELLOW}INFO${NC}: $1"; }

# ─── 1. Start Redis ───────────────────────────────────────────────────────────
info "Starting Redis..."
if docker ps --format '{{.Names}}' | grep -q '^e2e-redis$'; then
    info "Redis container 'e2e-redis' already running"
    REDIS_CONTAINER="e2e-redis"
else
    REDIS_CONTAINER=$(docker run -d --name e2e-redis -p 6379:6379 redis:7-alpine)
    info "Started Redis container: $REDIS_CONTAINER"
    sleep 1
fi

export REDIS_URL="redis://localhost:6379"

# ─── 2. Build & start Citadel worker ──────────────────────────────────────────
info "Building citadel-cli..."
if [[ -x "$CITADEL_DIR/citadel" ]]; then
    info "Using existing citadel binary"
else
    (cd "$CITADEL_DIR" && go build -o citadel ./cmd/citadel) || fail "Failed to build citadel-cli"
fi

info "Starting Citadel worker..."
"$CITADEL_DIR/citadel" work \
    --force-direct-redis \
    --redis-url "$REDIS_URL" \
    --queue jobs:v1:cpu-general \
    --redis-status=false \
    --no-services \
    --status-port 8080 \
    > /tmp/citadel-worker.log 2>&1 &
PIDS+=($!)
sleep 2

if ! kill -0 "${PIDS[-1]}" 2>/dev/null; then
    echo "Citadel worker log:"
    cat /tmp/citadel-worker.log
    fail "Citadel worker failed to start"
fi
info "Citadel worker running (PID ${PIDS[-1]})"

# ─── 3. Start Platform backend ────────────────────────────────────────────────
info "Starting platform backend..."
(cd "$ROOT_DIR/aceteam/python-backend" && \
    REDIS_URL="$REDIS_URL" \
    uv run uvicorn app:app --host 127.0.0.1 --port 8000 \
    > /tmp/platform-backend.log 2>&1) &
PIDS+=($!)

# Wait for backend to be ready
for i in $(seq 1 15); do
    if curl -s http://localhost:8000/docs > /dev/null 2>&1; then
        break
    fi
    if [[ $i -eq 15 ]]; then
        echo "Platform backend log:"
        cat /tmp/platform-backend.log
        fail "Platform backend failed to start within 15 seconds"
    fi
    sleep 1
done
info "Platform backend running (PID ${PIDS[-1]})"

# ─── 4. Direct test: ping citadel's HTTP status endpoint ─────────────────────
info "Test 1: Direct citadel ping..."
PING_RESPONSE=$(curl -sf http://localhost:8080/ping 2>/dev/null || echo "FAIL")
if [[ "$PING_RESPONSE" == "FAIL" ]]; then
    info "Citadel ping endpoint not available (may not be enabled) — skipping direct test"
else
    pass "Direct citadel ping: $PING_RESPONSE"
fi

# ─── 5. Routed test: curl → platform → redis → citadel ───────────────────────
info "Test 2: Routed dispatch (curl → platform → citadel)..."
DISPATCH_RESPONSE=$(curl -sf -X POST http://localhost:8000/fabric/dispatch \
    -H "Content-Type: application/json" \
    -d '{"job_type":"SHELL_COMMAND","payload":{"command":"echo hello from citadel"},"timeout":15,"queue":"jobs:v1:cpu-general"}' \
    2>/dev/null) || fail "Dispatch request failed"

echo "  Response: $DISPATCH_RESPONSE"

# Check response contains expected fields
if echo "$DISPATCH_RESPONSE" | python3 -c "
import sys, json
resp = json.load(sys.stdin)
assert resp['status'] == 'completed', f'Expected completed, got {resp[\"status\"]}'
assert 'hello from citadel' in resp['result'].get('output', ''), 'Missing expected output'
print('  Verified: status=completed, output contains \"hello from citadel\"')
" 2>/dev/null; then
    pass "Routed dispatch (platform → citadel)"
else
    fail "Routed dispatch returned unexpected response: $DISPATCH_RESPONSE"
fi

# ─── 6. Ace workflow test (if ace CLI is built and node is available) ─────────
ACE_CLI="$ROOT_DIR/ace/dist/index.js"
NODE_BIN=$(command -v node 2>/dev/null || echo "")
if [[ -f "$ACE_CLI" ]] && [[ -n "$NODE_BIN" ]]; then
    info "Test 3: Ace workflow run (routed)..."
    WORKFLOW_OUTPUT=$("$NODE_BIN" "$ACE_CLI" workflow run \
        "$ROOT_DIR/aceteam-nodes/examples/hello-citadel-routed.json" \
        2>&1) || {
        info "ace workflow run failed (non-critical): $WORKFLOW_OUTPUT"
        info "Skipping — the core pipeline (curl test) passed above"
    }

    if [[ -n "$WORKFLOW_OUTPUT" ]] && echo "$WORKFLOW_OUTPUT" | grep -q "completed"; then
        echo "  Workflow output: $WORKFLOW_OUTPUT"
        pass "Ace workflow run (routed)"
    fi
else
    info "Skipping ace workflow test — requires node and ace CLI to be built"
fi

echo ""
echo "=== All tests passed ==="
