package storage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// TestDockerRunArgs asserts the container is published on loopback only (never
// 0.0.0.0 and never the mesh IP), credentials go via env not argv, and the
// VersityGW posix command is assembled correctly against the pinned image.
func TestDockerRunArgs(t *testing.T) {
	creds := Credentials{AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t/secret+key"}
	args := dockerRunArgs(creds, services.StorageHostPort, "/home/x/.citadel/storage/data", "/home/x/.citadel/storage/iam")
	joined := strings.Join(args, " ")

	wantPublish := fmt.Sprintf("127.0.0.1:%d:%d", services.StorageHostPort, containerPort)
	if !containsPair(args, "-p", wantPublish) {
		t.Fatalf("expected loopback publish %q, got: %s", wantPublish, joined)
	}
	// Must never bind all interfaces.
	if strings.Contains(joined, "0.0.0.0") {
		t.Fatalf("args must not bind 0.0.0.0: %s", joined)
	}

	// Restart policy for reboot survival.
	if !containsPair(args, "--restart", "unless-stopped") {
		t.Fatalf("expected --restart unless-stopped: %s", joined)
	}

	// Credentials via env, not on the command line.
	if !containsPair(args, "-e", "ROOT_ACCESS_KEY="+creds.AccessKey) ||
		!containsPair(args, "-e", "ROOT_SECRET_KEY="+creds.SecretKey) {
		t.Fatalf("expected creds passed via -e env: %s", joined)
	}

	// Both mounts present.
	if !containsPair(args, "-v", "/home/x/.citadel/storage/data:"+containerDataDir) {
		t.Fatalf("expected data volume mount: %s", joined)
	}
	if !containsPair(args, "-v", "/home/x/.citadel/storage/iam:"+containerIAMDir) {
		t.Fatalf("expected iam volume mount: %s", joined)
	}

	// Pinned image, then the posix command with the backing dir last.
	if !strings.Contains(joined, Image) {
		t.Fatalf("expected pinned image %q: %s", Image, joined)
	}
	if !strings.HasSuffix(joined, "posix "+containerDataDir) {
		t.Fatalf("expected posix backing dir as final arg: %s", joined)
	}
	// The image must appear before the versitygw flags (so they are not read as
	// docker flags).
	imgIdx := indexOf(args, Image)
	posixIdx := indexOf(args, "posix")
	if imgIdx < 0 || posixIdx < 0 || imgIdx > posixIdx {
		t.Fatalf("image must precede the posix subcommand: %s", joined)
	}
}

// TestStorageHostPort_Allocation asserts the fixed host port is a well-behaved
// registry slot: outside citadel's reserved listeners, outside the apps
// auto-allocation range, and unique among the managed service host ports. This
// is the "port allocation" guarantee for the storage service.
func TestStorageHostPort_Allocation(t *testing.T) {
	port := HostPort()
	if port != services.StorageHostPort {
		t.Fatalf("HostPort() = %d, want registry slot %d", port, services.StorageHostPort)
	}

	// Not a citadel-reserved listener (gateway, status, VNC, terminal, ...).
	if name, reserved := services.ReservedCitadelPorts[port]; reserved {
		t.Fatalf("storage port %d collides with reserved citadel port %q", port, name)
	}

	// Outside the apps auto-allocation range so a dynamically allocated app can
	// never land on it.
	if port >= services.AppsPortRangeStart && port <= services.AppsPortRangeEnd {
		t.Fatalf("storage port %d falls inside the apps range %d-%d",
			port, services.AppsPortRangeStart, services.AppsPortRangeEnd)
	}

	// Unique among managed service host ports.
	for svc, p := range services.ServiceHostPorts {
		if svc == "storage" {
			continue
		}
		if p == port {
			t.Fatalf("storage port %d collides with managed service %q", port, svc)
		}
	}
}

// containsPair reports whether args contains flag immediately followed by value.
func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// indexOf returns the index of the first occurrence of v in args, or -1.
func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}
