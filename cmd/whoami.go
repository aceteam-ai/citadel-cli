// cmd/whoami.go
//
// `citadel whoami` (alias `citadel id`) answers "am I on a Citadel node, and
// what is its identity?" using LOCAL/persisted state -- no sudo, no live
// `citadel work`/`citadel up` process required. See aceteam #8139.
//
// KEY FINDING (originally verified for citadel#844; updated for aceteam
// #8139's persistence half -- docs/design-node-identity-receipts.md -- do
// not re-litigate without re-checking the code): there is still NO backend
// process that actually SENDS a numeric AceTeam fabric node ID to a node
// today, but there is now a place for one to land if it does.
//   - DeviceConfig.FabricNodeID (cmd/work.go) is read from the SAME
//     machine-convergent config.yaml as OrgID/OrgName/UserEmail/UserName,
//     and nexus.TokenResponse (internal/nexus/deviceauth.go) carries an
//     additive, inert FabricNodeID field for one candidate backend echo
//     point (device-auth /token). Until the backend populates one of the two
//     echo points the design doc leaves open, this reads empty -- same as
//     before, just with a real read/write path instead of no path at all.
//   - internal/heartbeat's on-disk marker (marker.go) tracks write freshness
//     only, no identity fields.
//   - The OTHER on-disk slot that looked intended for it --
//     SSHSyncConfig.NodeID ("Node ID in AceTeam platform",
//     internal/nexus/sshkeys.go) -- has no code path that ever writes it
//     (SaveSSHSyncConfig has zero non-test callers; only LoadSSHSyncConfig is
//     called, from cmd/run.go) and has a documented clobber trap besides
//     (design doc §1a) -- do not use it as a write target. whoami still
//     reads it as a documented last-resort fallback, below DeviceConfig.
//     FabricNodeID, so it lights up for free the day a backend process
//     starts populating it; today it reads empty on essentially every real
//     node.
//
// The one identifier that IS resolvable locally is the Headscale/mesh numeric
// node ID (network.NetworkStatus.NodeID) -- but only LIVE, from an active or
// reconnectable tsnet session (internal/network/server.go's Status() reads
// status.Self.ID off the running backend; nothing persists it to disk). To
// surface it, whoami performs the exact same saved-state reconnect probe
// `citadel status` already does (network.HasState -> VerifyOrReconnect ->
// GetGlobalStatus, same 5s bound) -- not a pure file read, but it needs
// neither sudo nor a separately-running worker, and it reuses citadel's
// existing userspace tsnet reconnect rather than inventing a new one.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

// identityNetworkTimeout bounds the live Headscale reconnect/status probe,
// mirroring the identical bound cmd/status.go uses for the same call.
const identityNetworkTimeout = 5 * time.Second

// identityCacheFile is the basename of the stable local identity cache
// (aceteam #8139) so an external agent/human can read a node's identity
// in one file read instead of re-deriving it. It lives in
// network.GetNodeConfigDir() -- the machine-convergent directory, not
// platform.ConfigDir() -- so a systemd-root `citadel work` and an
// interactive non-root `citadel whoami` agree on where to find it.
const identityCacheFile = "identity.json"

// NodeIdentity is the full self-identity report `citadel whoami` gathers and
// caches. Every field is best-effort: an unregistered or offline node still
// returns a valid NodeIdentity with the unavailable fields left empty and a
// note in Warnings, never an error.
type NodeIdentity struct {
	// NodeName is citadel.yaml's node.name (cmd/manifest.go's CitadelManifest),
	// the name set at `citadel init`/onboarding. Falls back to the
	// Headscale-reported hostname, then empty.
	NodeName string `json:"node_name,omitempty"`
	// Hostname is the OS hostname, always available regardless of
	// registration state.
	Hostname string `json:"hostname,omitempty"`

	// HeadscaleNodeID is the mesh/coordination-server numeric node ID
	// (Headscale's StableNodeID, network.NetworkStatus.NodeID). This is the
	// only fabric-adjacent identifier resolvable from this host today -- see
	// the package doc comment above. Empty when never connected, offline, or
	// the reconnect probe fails/times out.
	HeadscaleNodeID string `json:"headscale_node_id,omitempty"`
	// PlatformNodeID is the AceTeam-platform numeric node ID IF some backend
	// process has ever written it to ssh_sync.yaml (SSHSyncConfig.NodeID).
	// In practice this is empty on essentially every node today -- see the
	// package doc comment. NOT the same identifier as HeadscaleNodeID.
	PlatformNodeID string `json:"platform_node_id,omitempty"`

	Connected bool   `json:"connected"`
	MeshIPv4  string `json:"mesh_ipv4,omitempty"`
	MeshIPv6  string `json:"mesh_ipv6,omitempty"`

	OrgID     string `json:"org_id,omitempty"`
	OrgName   string `json:"org_name,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	UserName  string `json:"user_name,omitempty"`

	// Registered is true if this host has ANY persisted registration signal
	// (citadel.yaml manifest, device config, or saved network state). False
	// means `citadel init`/`citadel login` has not been run here (or state
	// was cleared) -- the "am I on a Citadel node at all" answer.
	Registered bool `json:"registered"`

	CitadelVersion string `json:"citadel_version"`

	// HeartbeatFreshness summarizes the last successful durable heartbeat
	// write (internal/heartbeat/marker.go): "ok", "stale (<age> ago)", or
	// "unknown" if no durable write has ever been recorded in this config
	// dir (fresh install, or no worker has run here yet).
	HeartbeatFreshness string `json:"heartbeat_freshness"`
	// LastHeartbeatAt is RFC3339, empty when HeartbeatFreshness is "unknown".
	LastHeartbeatAt string `json:"last_heartbeat_at,omitempty"`

	// NodeConfigDir is the machine-convergent config dir this report (and its
	// identity.json cache) was read from/written to (network.GetNodeConfigDir()).
	NodeConfigDir string `json:"node_config_dir,omitempty"`

	// Warnings surfaces degraded-but-non-fatal conditions (e.g. "not
	// connected to AceTeam Network", "no citadel.yaml manifest found").
	Warnings []string `json:"warnings,omitempty"`
}

// resolvePlatformNodeID picks whoami's platform_node_id: the device-config
// FabricNodeID (aceteam #8139, DeviceConfig.FabricNodeID via
// getDeviceConfigFromFile) when present, else the legacy
// SSHSyncConfig.NodeID fallback (design-node-identity-receipts.md §1a/§2).
// Split out as a pure function -- rather than left inline in gatherIdentity
// -- so the preference order is unit-testable without faking
// network.GetNodeConfigDir() or nexus.LoadSSHSyncConfig's file reads.
func resolvePlatformNodeID(fabricNodeID, sshSyncNodeID string) string {
	if fabricNodeID != "" {
		return fabricNodeID
	}
	return sshSyncNodeID
}

// gatherIdentity collects a NodeIdentity from local/persisted state, plus the
// one live network probe described in the package doc comment. It never
// errors and never panics on an unregistered/offline host -- every source is
// read best-effort and a miss becomes a Warnings entry, not a failure.
func gatherIdentity(ctx context.Context) NodeIdentity {
	id := NodeIdentity{
		CitadelVersion: Version,
	}
	if h, err := os.Hostname(); err == nil {
		id.Hostname = h
	}

	var manifestOrgID string
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		id.NodeName = manifest.Node.Name
		manifestOrgID = manifest.Node.OrgID
		id.Registered = true
	} else if err != nil {
		id.Warnings = append(id.Warnings, fmt.Sprintf("citadel.yaml not found: %v", err))
	}

	// OrgID/OrgName are taken from the SAME source together, never mixed: the
	// device config is the live auth artifact that carries them as a
	// coherent pair (org_id + org_name from the same /token response), so it
	// wins whenever present. Falling back to the manifest's OrgID alone (with
	// OrgName left empty) is safe -- orgDisplayString prints just the bare ID
	// in that case rather than pairing it with a name from a different
	// source, which could show a stale/mismatched name (e.g. after a re-init
	// against a different org, or an APPLY_DEVICE_CONFIG that only touched
	// one of the two files).
	if dc := getDeviceConfigFromFile(); dc != nil {
		id.Registered = true
		id.OrgID = dc.OrgID
		id.OrgName = dc.OrgName
		id.UserEmail = dc.UserEmail
		id.UserName = dc.UserName
		// FabricNodeID (aceteam #8139) is read here, alongside the other
		// fields from the SAME /token-response-derived file, rather than
		// SSHSyncConfig.NodeID below -- see design-node-identity-receipts.md
		// §2. It is empty on every node until a backend echo point starts
		// populating it; SSHSyncConfig remains a documented last-resort
		// fallback (see below) since it costs nothing to leave in place.
		id.PlatformNodeID = dc.FabricNodeID
	} else if manifestOrgID != "" {
		id.OrgID = manifestOrgID
	}

	// network.GetNodeConfigDir(), NOT platform.ConfigDir() -- this state must
	// agree between a systemd-root `citadel work` and an interactive
	// non-root `citadel whoami` (see the CLAUDE.md ConfigDir()/GetNodeConfigDir()
	// entry this file's doc comment is consistent with).
	nodeConfigDir := network.GetNodeConfigDir()
	id.NodeConfigDir = nodeConfigDir

	// Last-resort fallback ONLY: SSHSyncConfig.NodeID has no writer today
	// (design-node-identity-receipts.md §1a) and reads empty on essentially
	// every real node, but costs nothing to keep checking -- it lights up
	// for free if a backend process ever starts populating it, and the
	// device-config value above always wins when present.
	var sshSyncNodeID string
	if sshConfig, err := nexus.LoadSSHSyncConfig(nodeConfigDir); err == nil && sshConfig != nil {
		sshSyncNodeID = sshConfig.NodeID
	}
	id.PlatformNodeID = resolvePlatformNodeID(id.PlatformNodeID, sshSyncNodeID)

	if network.HasState() {
		id.Registered = true
		netCtx, cancel := context.WithTimeout(ctx, identityNetworkTimeout)
		defer cancel()

		connected, reconnectErr := network.VerifyOrReconnect(netCtx)
		id.Connected = connected

		if status, err := network.GetGlobalStatus(netCtx); err == nil {
			id.HeadscaleNodeID = status.NodeID
			id.MeshIPv4 = status.IPv4
			id.MeshIPv6 = status.IPv6
			if id.NodeName == "" {
				id.NodeName = status.Hostname
			}
		} else {
			id.Warnings = append(id.Warnings, fmt.Sprintf("could not read network status: %v", err))
		}

		if !connected {
			msg := "not currently connected to the AceTeam Network"
			if reconnectErr != nil {
				msg = fmt.Sprintf("%s: %v", msg, reconnectErr)
			}
			id.Warnings = append(id.Warnings, msg)
		}
	} else {
		id.Warnings = append(id.Warnings, "not connected to AceTeam Network (run 'citadel init' or 'citadel login')")
	}

	marker := heartbeat.LoadMarker(nodeConfigDir)
	state, age := heartbeat.Freshness(marker, time.Now(), heartbeat.DefaultStaleAfter)
	switch state {
	case heartbeat.FreshnessOK:
		id.HeartbeatFreshness = "ok"
		id.LastHeartbeatAt = marker.LastSuccessAt.Format(time.RFC3339)
	case heartbeat.FreshnessStale:
		id.HeartbeatFreshness = fmt.Sprintf("stale (%s ago)", age.Round(time.Second))
		id.LastHeartbeatAt = marker.LastSuccessAt.Format(time.RFC3339)
	default:
		id.HeartbeatFreshness = "unknown"
	}

	if !id.Registered {
		id.Warnings = append(id.Warnings, "this host does not appear to be a registered citadel node (no citadel.yaml, device config, or network state found)")
	}

	return id
}

// writeIdentityCache writes id as JSON to <nodeConfigDir>/identity.json,
// creating the directory if needed, so an external agent/human can read a
// node's identity in one file read (aceteam #8139). Idempotent: called on
// every `citadel whoami` invocation, always overwriting with the latest
// gather. Mode is forced to 0600 on every write (not just at creation, since
// os.WriteFile only applies its perm argument when creating the file) because
// the file has no secrets today but is co-located with ones that do
// (config.yaml, ssh_sync.yaml) in the same directory.
func writeIdentityCache(nodeConfigDir string, id NodeIdentity) error {
	if nodeConfigDir == "" {
		return fmt.Errorf("no node config dir resolved")
	}
	if err := os.MkdirAll(nodeConfigDir, 0755); err != nil {
		return fmt.Errorf("create node config dir: %w", err)
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	path := filepath.Join(nodeConfigDir, identityCacheFile)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, 0600)
}

func valueOrUnknown(s string) string {
	if s == "" {
		return faintColor.Sprint("unknown")
	}
	return s
}

// renderIdentity prints a human-readable report to w.
func renderIdentity(w io.Writer, id NodeIdentity) {
	headerColor.Fprintln(w, "--- 🪪 Citadel Node Identity ---")

	fmt.Fprintf(w, "\n%s %s\n", labelColor.Sprint("Node name:"), valueOrUnknown(id.NodeName))
	fmt.Fprintf(w, "%s %s\n", labelColor.Sprint("Hostname:"), valueOrUnknown(id.Hostname))
	fmt.Fprintf(w, "%s %s\n", labelColor.Sprint("Citadel version:"), id.CitadelVersion)

	headerColor.Fprintln(w, "\nFABRIC / NETWORK IDENTITY")
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Headscale node ID:"), valueOrUnknown(id.HeadscaleNodeID))
	if id.PlatformNodeID != "" {
		fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("AceTeam platform node ID:"), id.PlatformNodeID)
	} else {
		fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("AceTeam platform node ID:"), faintColor.Sprint("not available locally (not yet echoed by the backend, see aceteam #8139)"))
	}
	connStr := badColor.Sprint("offline")
	if id.Connected {
		connStr = goodColor.Sprint("online")
	}
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Network status:"), connStr)
	if id.MeshIPv4 != "" {
		fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Mesh IP:"), id.MeshIPv4)
	}

	headerColor.Fprintln(w, "\nORGANIZATION / USER")
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Org:"), valueOrUnknown(orgDisplayString(id)))
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("User:"), valueOrUnknown(userDisplayString(id)))

	headerColor.Fprintln(w, "\nHEARTBEAT")
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Freshness:"), id.HeartbeatFreshness)

	if len(id.Warnings) > 0 {
		headerColor.Fprintln(w, "\nWARNINGS")
		for _, msg := range id.Warnings {
			fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[!]"), msg)
		}
	}

	fmt.Fprintln(w)
	if id.Registered {
		goodColor.Fprintln(w, "Overall: this host is a registered citadel node")
	} else {
		badColor.Fprintln(w, "Overall: this host is NOT a registered citadel node")
	}
}

// orgDisplayString formats org name + ID for the human report, preferring
// the friendlier name and appending the ID in parens when both are known.
func orgDisplayString(id NodeIdentity) string {
	switch {
	case id.OrgName != "" && id.OrgID != "":
		return fmt.Sprintf("%s (%s)", id.OrgName, id.OrgID)
	case id.OrgName != "":
		return id.OrgName
	default:
		return id.OrgID
	}
}

// userDisplayString formats the device-auth user identity for the human report.
func userDisplayString(id NodeIdentity) string {
	switch {
	case id.UserName != "" && id.UserEmail != "":
		return fmt.Sprintf("%s <%s>", id.UserName, id.UserEmail)
	case id.UserEmail != "":
		return id.UserEmail
	default:
		return id.UserName
	}
}

var whoamiJSON bool

var whoamiCmd = &cobra.Command{
	Use:     "whoami",
	Aliases: []string{"id"},
	Short:   "Show this node's self-identity (name, mesh ID, org, version)",
	Long: `citadel whoami answers "am I on a Citadel node, and what is its identity?"
using local/persisted state. It requires neither sudo nor a live
'citadel work'/'citadel up' process -- an unregistered or offline host still
prints a well-formed report with the unavailable fields called out.

It also refreshes a stable local cache at
<node config dir>/identity.json (network.GetNodeConfigDir()) so an agent or
script can read a node's identity with a single file read instead of
re-deriving it every time.

Note: there is currently no numeric AceTeam fabric/platform node ID available
locally -- only the mesh (Headscale) node ID can be resolved from this host.
See aceteam #8139.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		id := gatherIdentity(cmd.Context())

		// citadel#853: NodeName above may have come from an overridden
		// manifest (--node-dir/CITADEL_NODE_DIR), but id.NodeConfigDir is
		// network.GetNodeConfigDir() -- the REAL machine's node config dir,
		// not the override -- since --node-dir intentionally does not
		// redirect network/mesh state. Writing the cache here would overwrite
		// the real machine's identity.json with an overridden node's name, a
		// cross-context split the same class as the ConfigDir()/
		// GetNodeConfigDir() hazard this file's package doc warns about.
		if override := resolveNodeDirOverride(); override != "" {
			id.Warnings = append(id.Warnings, fmt.Sprintf(
				"--node-dir/CITADEL_NODE_DIR override is active (%s): identity.json cache write skipped "+
					"to avoid overwriting this machine's real identity cache with overridden data", override))
		} else if err := writeIdentityCache(id.NodeConfigDir, id); err != nil {
			id.Warnings = append(id.Warnings, fmt.Sprintf("could not write identity cache: %v", err))
		}

		if whoamiJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(id)
		}
		renderIdentity(cmd.OutOrStdout(), id)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(whoamiCmd)
	whoamiCmd.Flags().BoolVar(&whoamiJSON, "json", false, "Output in JSON format")
}
