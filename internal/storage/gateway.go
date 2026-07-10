package storage

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
)

// Image is the pinned VersityGW image. It is a BUILT-IN, operator-vetted
// reference baked into the binary, NOT a remotely-supplied one, so it is
// deliberately exempt from the internal/jobs SERVICE_START registry allowlist
// (which constrains untrusted inline payload images to ghcr.io/aceteam-ai/).
// VersityGW's official image lives on Docker Hub; pinning an exact tag here is
// the trust boundary. Bump this constant to upgrade.
const Image = "versity/versitygw:v1.6.0"

// ContainerName is the docker container name for the storage gateway. The
// "citadel-" prefix matches the manifest/payload service convention.
const ContainerName = "citadel-storage"

// containerPort is VersityGW's in-container S3 listen port. The host publish
// (services.StorageHostPort) maps to this.
const containerPort = 7070

// containerDataDir / containerIAMDir are the in-container mount targets for the
// posix backing dir and the gateway's IAM state.
const (
	containerDataDir = "/data"
	containerIAMDir  = "/iam"
)

// HostPort is the fixed host port the gateway publishes on.
func HostPort() int { return services.StorageHostPort }

// dockerRunArgs assembles the `docker run` arguments for the gateway. Pure
// (inputs in, args out) so the port/bind/volume/env/command wiring is testable
// without Docker.
//
// Binding: the container is published on 127.0.0.1 ONLY. It is never bound to
// 0.0.0.0, so it is not exposed on the LAN or publicly. It is also not bound to
// the node's mesh IP: that address is a userspace-tsnet virtual IP with no
// kernel interface, so `docker -p <mesh-ip>:...` cannot bind it. Mesh
// reachability for S3 needs a TCP-preserving relay (a path-rewriting gateway
// route would break SigV4, which signs host+path); that is deferred past M1.
//
// Credentials are passed as environment (ROOT_ACCESS_KEY/ROOT_SECRET_KEY), not
// on the command line, so the secret never appears in the container's argv.
// VersityGW args are passed explicitly (the image entrypoint execs the binary
// with "$@"), which is stable across image versions.
func dockerRunArgs(creds Credentials, hostPort int, dataDir, iamDir string) []string {
	args := []string{
		"run", "-d",
		"--name", ContainerName,
		// Survive node reboot with no citadel loop (nothing else auto-starts
		// this). STOP does an explicit `docker rm`, so this never resurrects a
		// deliberately stopped gateway.
		"--restart", "unless-stopped",
		// Loopback publish only. Never 0.0.0.0.
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort),
		"-v", fmt.Sprintf("%s:%s", dataDir, containerDataDir),
		"-v", fmt.Sprintf("%s:%s", iamDir, containerIAMDir),
		"-e", "ROOT_ACCESS_KEY=" + creds.AccessKey,
		"-e", "ROOT_SECRET_KEY=" + creds.SecretKey,
		Image,
		// Global flags precede the posix subcommand; posix takes the backing dir.
		"--port", fmt.Sprintf(":%d", containerPort),
		"--iam-dir", containerIAMDir,
		"posix", containerDataDir,
	}
	return args
}

// Status is the reported state of the storage gateway.
type Status struct {
	Running     bool   `json:"running"`
	Healthy     bool   `json:"healthy"`
	Endpoint    string `json:"endpoint"`
	MeshIP      string `json:"mesh_ip"`
	Port        int    `json:"port"`
	BucketCount int    `json:"bucket_count"`
	BytesUsed   int64  `json:"bytes_used"`
	AccessKey   string `json:"access_key"`
}

// Start launches the gateway container. It is idempotent: if the container is
// already running it returns without error. Credentials are create-once (loaded
// or minted by LoadOrCreateState); the backing dir is bounded to the storage
// data dir.
func Start() error {
	if err := dockerAvailable(); err != nil {
		return err
	}
	state, err := LoadOrCreateState()
	if err != nil {
		return fmt.Errorf("failed to load storage state: %w", err)
	}

	if containerRunning() {
		return nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve home directory: %w", err)
	}
	dataDir, err := resolveBackingDir("", homeDir)
	if err != nil {
		return err
	}
	iamDir, err := IAMDir()
	if err != nil {
		return err
	}
	// 0700: the backing dirs hold object data and IAM state.
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create backing dir %s: %w", dataDir, err)
	}
	if err := os.MkdirAll(iamDir, 0700); err != nil {
		return fmt.Errorf("failed to create iam dir %s: %w", iamDir, err)
	}

	// A stale stopped container with the same name blocks `docker run --name`;
	// remove it first (best-effort). The backing dirs on disk are untouched, so a
	// relaunch reattaches the same durable state and credentials.
	_ = exec.Command("docker", "rm", "-f", ContainerName).Run()

	args := dockerRunArgs(state.Credentials, HostPort(), dataDir, iamDir)
	if out, runErr := exec.Command("docker", args...).CombinedOutput(); runErr != nil {
		return fmt.Errorf("failed to launch storage gateway: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop stops and removes the gateway container. The backing dirs (objects, IAM,
// credentials) are left intact so a later Start reattaches them.
func Stop() error {
	if !containerRunning() && !containerExists() {
		return nil
	}
	if out, err := exec.Command("docker", "rm", "-f", ContainerName).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to stop storage gateway: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// GetStatus reports the gateway's current state: endpoint, mesh IP, port, bucket
// count, bytes used, and health.
func GetStatus() (*Status, error) {
	state, err := loadState()
	if err != nil {
		return nil, err
	}
	running := containerRunning()
	port := HostPort()

	st := &Status{
		Running:   running,
		Endpoint:  fmt.Sprintf("http://127.0.0.1:%d", port),
		MeshIP:    meshIP(),
		Port:      port,
		AccessKey: state.Credentials.AccessKey,
	}

	dataDir, derr := DataDir()
	if derr == nil {
		if count, bytes, aerr := usage(dataDir); aerr == nil {
			st.BucketCount = count
			st.BytesUsed = bytes
		}
	}

	if running {
		st.Healthy = healthy(st.Endpoint)
	}
	return st, nil
}

// usage reports the bucket count and bytes used under the posix backing dir. In
// the posix backend each bucket is a top-level directory and each object is a
// regular file, so buckets are the non-dotfile immediate subdirectories and
// bytes-used is the sum of regular-file sizes. These are filesystem-level
// approximations (no S3 auth needed) and are documented as such in status.
func usage(dataDir string) (int, int64, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	buckets := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			buckets++
		}
	}

	var total int64
	_ = filepath.WalkDir(dataDir, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // best-effort: skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return buckets, total, nil
}

// healthy reports whether the gateway answers HTTP. VersityGW returns an S3 XML
// error (e.g. 403 AccessDenied) to an anonymous request, which is still a valid
// HTTP response and proves the listener is up. A connection refused/timeout is
// unhealthy.
func healthy(endpoint string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// meshIP returns the node's AceTeam Network IPv4 best-effort. In a one-shot CLI
// invocation the embedded tsnet server is not running, so this is typically ""
// unless the command runs inside a connected process; it is informational only.
func meshIP() string {
	ip, err := network.GetGlobalIPv4()
	if err != nil {
		return ""
	}
	return ip
}

// dockerAvailable checks that the docker daemon is responsive.
func dockerAvailable() error {
	if err := exec.Command("docker", "info").Run(); err != nil {
		return fmt.Errorf("Docker is not available (is Docker installed and running?): %w", err)
	}
	return nil
}

// containerRunning reports whether the gateway container is running.
func containerRunning() bool {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", ContainerName).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "running"
}

// containerExists reports whether the gateway container exists in any state.
func containerExists() bool {
	return exec.Command("docker", "inspect", "--format", "{{.Name}}", ContainerName).Run() == nil
}
