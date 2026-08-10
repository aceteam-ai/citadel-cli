package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The bug in aceteam-ai/citadel-cli#718 was a missing argv: `docker compose ...
// up -d` with no pull, against a floating `:latest` tag, is a no-op on an image
// that is already present locally -- so a provisioned bridge could never be
// upgraded, and the provision reported success anyway. These tests therefore
// assert the argv itself, through the runBridgeCompose seam, without Docker.

// recordCompose swaps runBridgeCompose for a recorder and restores it on
// cleanup. errFor lets a test fail a specific subcommand (matched on the first
// non-flag word after the -p/-f/--env-file preamble).
func recordCompose(t *testing.T, errFor map[string]error) *[][]string {
	t.Helper()
	prev := runBridgeCompose
	var calls [][]string
	runBridgeCompose = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if err, ok := errFor[composeSubcommand(args)]; ok {
			return []byte("boom"), err
		}
		return []byte(""), nil
	}
	t.Cleanup(func() { runBridgeCompose = prev })
	return &calls
}

// composeSubcommand extracts the compose subcommand ("pull", "up", "down") from
// a recorded argv.
func composeSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "compose", "-d":
			continue
		case "-p", "-f", "--env-file":
			i++ // skip the flag's value
			continue
		default:
			return args[i]
		}
	}
	return ""
}

func TestBridgeComposeArgsCarryProjectFileAndEnv(t *testing.T) {
	got := bridgeComposeArgs("services", "/n/services/whatsapp-bridge.yml", "/n/services/whatsapp-bridge.env", "pull", "bridge")
	want := []string{
		"compose", "-p", "services",
		"-f", "/n/services/whatsapp-bridge.yml",
		"--env-file", "/n/services/whatsapp-bridge.env",
		"pull", "bridge",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v\nwant  %v", got, want)
	}
}

// TestStartBridgeStackPullsBeforeUp is the regression test for #718: a pull must
// precede the up, and both must name the `bridge` service.
func TestStartBridgeStackPullsBeforeUp(t *testing.T) {
	calls := recordCompose(t, nil)
	report := &deployReport{}

	if err := startBridgeStack(context.Background(), "services", "/n/wa.yml", "/n/wa.env", report); err != nil {
		t.Fatalf("startBridgeStack() error = %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("compose invocations = %d (%v), want 2 (pull then up)", len(*calls), *calls)
	}

	pull, up := (*calls)[0], (*calls)[1]
	if composeSubcommand(pull) != "pull" {
		t.Errorf("first invocation = %v, want the image pull FIRST (an up-only deploy is the #718 bug)", pull)
	}
	if composeSubcommand(up) != "up" {
		t.Errorf("second invocation = %v, want the up", up)
	}
	if pull[len(pull)-1] != "bridge" {
		t.Errorf("pull argv = %v, want it scoped to the bridge service", pull)
	}
	// Scoping the pull matters: an unscoped pull would also refresh
	// postgres:16-alpine and let the following up recreate the sidecar holding
	// the Baileys auth state.
	for _, a := range pull {
		if a == "--include-deps" {
			t.Errorf("pull argv = %v, must NOT pull dependencies (the Postgres sidecar)", pull)
		}
		// --ignore-pull-failures would make compose exit 0 on a failed pull,
		// destroying the signal this change exists to surface.
		if a == "--ignore-pull-failures" {
			t.Errorf("pull argv = %v, must NOT swallow pull failures", pull)
		}
	}
	if up[len(up)-1] != "bridge" {
		t.Errorf("up argv = %v, want it scoped to the bridge service", up)
	}
	for _, a := range up {
		if a == "--remove-orphans" {
			t.Fatalf("up argv = %v, must NEVER pass --remove-orphans: the `services` project is shared and it would delete the node's other modules", up)
		}
	}
	if report.PullError() != "" {
		t.Errorf("PullError = %q, want empty on a successful pull", report.PullError())
	}
}

// TestStartBridgeStackPullFailureIsNonFatalButRecorded: a node without
// `docker login` for the private registry must still start its cached image
// (no regression), while the failure stays visible in the result.
func TestStartBridgeStackPullFailureIsNonFatalButRecorded(t *testing.T) {
	calls := recordCompose(t, map[string]error{"pull": errors.New("exit status 1")})
	report := &deployReport{}

	if err := startBridgeStack(context.Background(), "services", "/n/wa.yml", "/n/wa.env", report); err != nil {
		t.Fatalf("startBridgeStack() error = %v, want a pull failure to be non-fatal", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("compose invocations = %d, want the up to still run after a failed pull", len(*calls))
	}
	pullErr := report.PullError()
	if pullErr == "" {
		t.Fatal("PullError = \"\", want the failure recorded (a silent failed pull is the false-green #718 is about)")
	}
	if !strings.Contains(pullErr, "docker login") {
		t.Errorf("PullError = %q, want it to keep the registry-reachability hint", pullErr)
	}
}

// TestStartBridgeStackUpFailureIsFatal: only the pull is best-effort. A failed
// `up` must still fail the deploy.
func TestStartBridgeStackUpFailureIsFatal(t *testing.T) {
	recordCompose(t, map[string]error{"up": errors.New("exit status 1")})

	err := startBridgeStack(context.Background(), "services", "/n/wa.yml", "/n/wa.env", &deployReport{})
	if err == nil {
		t.Fatal("startBridgeStack() error = nil, want the up failure to be fatal")
	}
	if !strings.Contains(err.Error(), "docker info") {
		t.Errorf("error = %q, want it to keep the 'is Docker running' hint", err)
	}
}

// TestBridgeComposeEnvIgnoresOrphans: the bridge shares the `services` compose
// project with every other module on the node, so compose reports those siblings
// as orphans on every up. Naming the `bridge` service does not suppress that
// (compose derives orphans from the loaded file's services), so the env var is
// what silences it -- and --remove-orphans must never be the answer.
func TestBridgeComposeEnvIgnoresOrphans(t *testing.T) {
	found := false
	for _, kv := range bridgeComposeEnv() {
		if kv == "COMPOSE_IGNORE_ORPHANS=true" {
			found = true
		}
	}
	if !found {
		t.Error("bridgeComposeEnv() must set COMPOSE_IGNORE_ORPHANS=true")
	}
}

func TestShortImageID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"sha256:af88f094aabbccddeeff", "af88f094aabb"},
		{"af88f094aabbccddeeff", "af88f094aabb"},
		{"sha256:short", "short"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := shortImageID(tt.in); got != tt.want {
			t.Errorf("shortImageID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
