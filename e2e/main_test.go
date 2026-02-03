// Package e2e contains end-to-end integration tests for AceTeam Fabric
package e2e

import (
	"log"
	"os"
	"testing"
)

// TestMain provides setup and teardown for all tests
func TestMain(m *testing.M) {
	// Log environment
	log.Printf("E2E Test Environment:")
	log.Printf("  ACETEAM_URL: %s", getEnvOrDefault("ACETEAM_URL", "http://localhost:3000"))
	log.Printf("  REDIS_URL: %s", getEnvOrDefault("REDIS_URL", "redis://localhost:6379"))
	log.Printf("  WORKER_QUEUE: %s", getEnvOrDefault("WORKER_QUEUE", "jobs:v1:e2e-test"))
	log.Printf("  CITADEL_BINARY: %s", os.Getenv("CITADEL_BINARY"))

	// Run tests
	code := m.Run()

	os.Exit(code)
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
