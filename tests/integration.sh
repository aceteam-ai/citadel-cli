#!/bin/bash
# tests/integration.sh

# This script performs a basic integration test of the citadel CLI.
# It tests the run -> status -> logs -> stop lifecycle.

set -e

echo "--- 🧪 Starting Citadel Integration Test ---"

# --- Setup ---
echo "--- 1. Setup: Cleaning up previous runs ---"
docker compose -f docker-compose.test.yml down --remove-orphans > /dev/null 2>&1 || true

echo "   - Creating test citadel.yaml"
cat > citadel.yaml << EOF
node:
  name: integration-test-node
  tags: []
services:
  - name: test-service
    compose_file: docker-compose.test.yml
EOF

# --- Test Execution ---
echo "--- 2. Execution: Running 'citadel run' ---"
./citadel run --force

echo "   - Verifying container status with docker..."
if ! docker ps | grep -q "citadel-test-service"; then
    echo "   ❌ FAILED: 'citadel run' ran, but test container is not running."
    exit 1
fi
echo "   ✅ Container is running."

echo "--- 3. Execution: Running 'citadel status' ---"
# Grep for the SERVICE name ("test-service"), not the CONTAINER name.
if ! ./citadel status | grep -q "test-service"; then
    echo "   ❌ FAILED: 'citadel status' did not report the running test service."
    exit 1
fi
echo "   ✅ Status command reported service correctly."

echo "--- 4. Execution: Running 'citadel logs' ---"
if ! ./citadel logs test-service --tail 50 | grep -q "Hello from Docker!"; then
    echo "   ❌ FAILED: 'citadel logs' did not retrieve the expected log message."
    ./citadel logs test-service --tail 50
    exit 1
fi
echo "   ✅ Logs command retrieved logs correctly."

echo "--- 5. Execution: Running 'citadel stop' ---"
./citadel stop

echo "   - Verifying container is stopped..."
if docker ps | grep -q "citadel-test-service"; then
    echo "   ❌ FAILED: 'citadel stop' ran, but test container is still running."
    exit 1
fi
echo "   ✅ Container is stopped."


# --- Cleanup ---
echo "--- 6. Cleanup: Removing test files ---"
rm citadel.yaml

echo ""
echo "--- ✅✅✅ Citadel Integration Test Passed! ---"
