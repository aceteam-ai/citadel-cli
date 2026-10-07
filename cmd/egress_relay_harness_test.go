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
		"NODE_PID_FINGERPRINT[$label]",
		"NODE_TERMINATION_UNCONFIRMED[$who]",
		"refusing to signal reused or unverified",
		"process remained live after KILL",
		"retaining its guarded home for retry",
		"public IP response is not globally routable",
		`${NODE_HOME[$who]}/.${label}.log`,
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
		"tr -d '[:space:]'",
		`/tmp/egress-relay-test-${RUN_ID}-${label}.log`,
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

func TestEgressRelayHarnessRefusesReusedPID(t *testing.T) {
	source, err := os.ReadFile(egressRelayHarnessPath(t))
	if err != nil {
		t.Fatal(err)
	}
	const mainMarker = "if ((DRY_RUN == 1)); then"
	prefix, _, ok := strings.Cut(string(source), mainMarker)
	if !ok {
		t.Fatalf("harness main marker %q not found", mainMarker)
	}

	// Execute the real helper definitions with a hostile stale PID record. The
	// cleanup must refuse the signal and leave the unrelated process alive.
	probe := prefix + `
trap - EXIT INT TERM
sleep 30 &
pid=$!
NODE_HOST[probe]=""
NODE_PID[hostile]="$pid"
NODE_PID_ROLE[hostile]=probe
NODE_PID_FINGERPRINT[hostile]="deliberately-stale-fingerprint"
if kill_bg hostile; then
  echo "stale fingerprint unexpectedly accepted" >&2
  kill -TERM "$pid" 2>/dev/null || true
  exit 71
fi
if ! kill -0 "$pid" 2>/dev/null; then
  echo "unrelated process was signaled" >&2
  exit 72
fi
kill -TERM "$pid"
wait "$pid" 2>/dev/null || true
`
	cmd := exec.Command("bash", "-c", probe)
	cmd.Env = append(os.Environ(), "CITADEL_TEST_AUTHKEY=", "CITADEL_TEST_AUTHKEY_CLIENT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PID-reuse probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "refusing to signal reused or unverified") {
		t.Fatalf("PID-reuse refusal missing:\n%s", out)
	}
}

func egressRelayHarnessPrefix(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile(egressRelayHarnessPath(t))
	if err != nil {
		t.Fatal(err)
	}
	const mainMarker = "if ((DRY_RUN == 1)); then"
	prefix, _, ok := strings.Cut(string(source), mainMarker)
	if !ok {
		t.Fatalf("harness main marker %q not found", mainMarker)
	}
	return prefix
}

func runEgressRelayHarnessProbe(t *testing.T, body string, env ...string) (string, error) {
	t.Helper()
	probe := egressRelayHarnessPrefix(t) + "\ntrap - EXIT INT TERM\n" + body
	cmd := exec.Command("bash", "-c", probe)
	cmd.Env = append(os.Environ(), "CITADEL_TEST_AUTHKEY=", "CITADEL_TEST_AUTHKEY_CLIENT=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestEgressRelayHarnessRetainsRecoveryStateWhenTerminationUnconfirmed(t *testing.T) {
	for _, mode := range []string{
		"transport",
		"failed-term",
		"post-term-transport",
		"failed-kill",
		"post-kill-transport",
		"live-after-kill",
	} {
		t.Run(mode, func(t *testing.T) {
			body := `
sleep() { :; }
run_on() {
  local who="$1"; shift
  case "$1" in
    sh)
      [[ "$TEST_MODE" == transport ]] && return 255
	  [[ "$TEST_MODE" == post-term-transport && "${TERM_SENT:-0}" == 1 ]] && return 255
	  [[ "$TEST_MODE" == post-kill-transport && "${KILL_SENT:-0}" == 1 ]] && return 255
      printf '%s' stable-fingerprint
      return 0
      ;;
    kill)
      if [[ "$2" == -TERM ]]; then
        [[ "$TEST_MODE" == failed-term ]] && return 1
		TERM_SENT=1
        return 0
      fi
      [[ "$TEST_MODE" == failed-kill ]] && return 1
	  KILL_SENT=1
      return 0
      ;;
    rm)
      echo "unexpected log removal" >&2
      return 91
      ;;
  esac
  return 92
}
node_cmd_stdin() {
  echo "unexpected deregistration" >&2
  return 93
}
NODE_PID[client-proxy]=4242
NODE_PID_ROLE[client-proxy]=client
NODE_PID_FINGERPRINT[client-proxy]=stable-fingerprint
NODE_LOG[client-proxy]=/tmp/egress-relay-test-client.RECOVER/.client-proxy.log
NODE_HOME[client]=/tmp/egress-relay-test-client.RECOVER
NODE_BIN[client]=/tmp/egress-relay-test-client.RECOVER/citadel
LOGIN_CLEANUP_REQUIRED[client]=1
if cleanup; then
  echo "cleanup unexpectedly succeeded" >&2
  exit 80
fi
[[ "${NODE_PID[client-proxy]:-}" == 4242 ]] || { echo "PID recovery state was discarded" >&2; exit 81; }
[[ "${NODE_PID_FINGERPRINT[client-proxy]:-}" == stable-fingerprint ]] || { echo "fingerprint recovery state was discarded" >&2; exit 82; }
[[ "${NODE_HOME[client]:-}" == /tmp/egress-relay-test-client.RECOVER ]] || { echo "HOME recovery state was discarded" >&2; exit 83; }
[[ "${LOGIN_CLEANUP_REQUIRED[client]:-}" == 1 ]] || { echo "registration recovery state was discarded" >&2; exit 84; }
[[ "${NODE_TERMINATION_UNCONFIRMED[client]:-}" == 1 ]] || { echo "unconfirmed termination was not recorded" >&2; exit 85; }
`
			out, err := runEgressRelayHarnessProbe(t, body, "TEST_MODE="+mode)
			if err != nil {
				t.Fatalf("%s cleanup probe failed: %v\n%s", mode, err, out)
			}
			if !strings.Contains(out, "retaining PID/fingerprint, registration, logs, and guarded HOME") {
				t.Fatalf("%s did not report retained recovery state:\n%s", mode, out)
			}
		})
	}
}

func TestEgressRelayHarnessDistinguishesTrueProcessAbsence(t *testing.T) {
	body := `
run_on() {
  local who="$1"; shift
  [[ "$1" == sh ]] || { echo "absence path attempted a signal" >&2; return 90; }
  return 3
}
NODE_PID[probe]=4242
NODE_PID_ROLE[probe]=client
NODE_PID_FINGERPRINT[probe]=stable-fingerprint
kill_bg probe
[[ -z "${NODE_PID[probe]:-}" ]] || { echo "absent PID was retained" >&2; exit 81; }
[[ -z "${NODE_PID_FINGERPRINT[probe]:-}" ]] || { echo "absent fingerprint was retained" >&2; exit 82; }
[[ "${NODE_TERMINATION_UNCONFIRMED[client]:-0}" == 0 ]] || { echo "absence marked unconfirmed" >&2; exit 83; }
`
	out, err := runEgressRelayHarnessProbe(t, body)
	if err != nil {
		t.Fatalf("true-absence probe failed: %v\n%s", err, out)
	}
}

func TestEgressRelayHarnessConfirmsVerifiedProcessDisappears(t *testing.T) {
	body := `
bash -c 'trap "" TERM; exec sleep 30' &
pid=$!
sleep 0.1
fingerprint="$(run_on client ps -p "$pid" -o lstart= -o args=)"
[[ -n "$fingerprint" ]] || { echo "could not fingerprint test process" >&2; exit 80; }
NODE_PID[probe]="$pid"
NODE_PID_ROLE[probe]=client
NODE_PID_FINGERPRINT[probe]="$fingerprint"
if ! kill_bg probe; then
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  echo "verified process cleanup failed" >&2
  exit 81
fi
wait "$pid" 2>/dev/null || true
if kill -0 "$pid" 2>/dev/null; then
  echo "verified process remains live" >&2
  kill -KILL "$pid" 2>/dev/null || true
  exit 82
fi
[[ -z "${NODE_PID[probe]:-}" && -z "${NODE_PID_FINGERPRINT[probe]:-}" ]] || { echo "successful cleanup retained stale state" >&2; exit 83; }
`
	out, err := runEgressRelayHarnessProbe(t, body)
	if err != nil {
		t.Fatalf("verified-process cleanup probe failed: %v\n%s", err, out)
	}
}

func TestEgressRelayHarnessRequiresOneGloballyRoutableIP(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
		ok      bool
	}{
		{name: "public IPv4 with outer whitespace", payload: " \t8.8.8.8\r\n", want: "8.8.8.8", ok: true},
		{name: "public IPv6 with outer whitespace", payload: "\n2001:4860:4860::8888 \t", want: "2001:4860:4860::8888", ok: true},
		{name: "multiple lines", payload: "8.8.8.8\n1.1.1.1"},
		{name: "interior whitespace", payload: "8.8. 8.8"},
		{name: "loopback IPv4", payload: "127.0.0.1"},
		{name: "private IPv4", payload: "10.0.0.1"},
		{name: "link local IPv4", payload: "169.254.1.2"},
		{name: "multicast IPv4", payload: "224.0.0.1"},
		{name: "documentation IPv4", payload: "192.0.2.10"},
		{name: "reserved IPv4", payload: "240.0.0.1"},
		{name: "unspecified IPv4", payload: "0.0.0.0"},
		{name: "loopback IPv6", payload: "::1"},
		{name: "private IPv6", payload: "fd00::1"},
		{name: "link local IPv6", payload: "fe80::1"},
		{name: "multicast IPv6", payload: "ff02::1"},
		{name: "documentation IPv6", payload: "2001:db8::1"},
		{name: "reserved IPv6", payload: "100::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `
run_on() { printf '%s' "$MOCK_IP_RESPONSE"; }
fetch_ip client
`
			out, err := runEgressRelayHarnessProbe(t, body, "MOCK_IP_RESPONSE="+tt.payload)
			if tt.ok {
				if err != nil {
					t.Fatalf("valid public address rejected: %v\n%s", err, out)
				}
				lines := strings.Split(strings.TrimSpace(out), "\n")
				got := lines[len(lines)-1]
				if got != tt.want {
					t.Fatalf("normalized address = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("hostile response %q was accepted as %q", tt.payload, out)
			}
			if !strings.Contains(out, "public IP response") {
				t.Fatalf("hostile response lacked a bounded diagnostic: %q", out)
			}
		})
	}
}
