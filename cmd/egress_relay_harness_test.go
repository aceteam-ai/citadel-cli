package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func egressRelayHarnessPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "scripts", "egress-relay-test.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat harness: %v", err)
	}
	return path
}

func TestEgressRelayHarnessPinsSafeCurrentContracts(t *testing.T) {
	source, err := os.ReadFile(egressRelayHarnessPath(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		"login --authkey-stdin",
		"egress-relay serve",
		"egress-relay allow-lan on",
		"egress-relay allow-lan off",
		"egress relay: authorized ${CLIENT_MESH_IP}:",
		"CITADEL_TEST_EXPECT_ORG_ID",
		"env -i",
		"curl --noproxy '*'",
		"LOGIN_CLEANUP_REQUIRED[$who]=1",
		"logout --force --api-key-stdin --require-deregister",
		"CITADEL_TEST_API_KEY_FILE",
		"loopback destination denied (allow_lan is off)",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("harness lost required contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"debug-redis-url",
		"login --authkey '$",
		`login --authkey "$`,
		"LOGIN_DONE",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("harness contains stale/unsafe contract %q", forbidden)
		}
	}
	if got := strings.Count(text, `assert_tunnel_healthy "the `); got != 2 {
		t.Errorf("policy refusals must each be followed by a public tunnel health assertion; got %d calls", got)
	}
	if got := strings.Count(text, `policy_denial_logged_after "$DENIALS_BEFORE"`); got != 2 {
		t.Errorf("policy refusals must each require a policy-specific log entry; got %d calls", got)
	}
}

func TestEgressRelayHarnessDryRunAndLegacySecretRefusal(t *testing.T) {
	path := egressRelayHarnessPath(t)
	cmd := exec.Command("bash", path, "--dry-run")
	cmd.Env = append(os.Environ(), "CITADEL_TEST_AUTHKEY=", "CITADEL_TEST_AUTHKEY_CLIENT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	for _, want := range []string{"no SSH, enrollment, process, or file mutation", "credentials streamed from 0600 files", "deny-LAN -> allow-LAN -> deny-LAN"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	const sentinel = "do-not-print-this-authkey"
	cmd = exec.Command("bash", path, "--dry-run")
	cmd.Env = append(os.Environ(), "CITADEL_TEST_AUTHKEY="+sentinel, "CITADEL_TEST_AUTHKEY_CLIENT=")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("legacy secret environment was accepted:\n%s", out)
	}
	if strings.Contains(string(out), sentinel) {
		t.Fatalf("legacy secret value was printed:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing legacy authkey environment") {
		t.Fatalf("unexpected refusal:\n%s", out)
	}
}
