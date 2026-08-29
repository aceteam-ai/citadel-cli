package config

import (
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

// deviceCredsFile is the global device-auth config written by `citadel init` /
// device authorization. It is the SAME file cmd.getDeviceConfigFromFile reads;
// this loader deliberately decodes only the two fields a background handler
// needs (the device API token + the API base URL) so a handler in internal/
// can authenticate against the AceTeam backend without importing cmd.
const deviceCredsFile = "config.yaml"

// DeviceConfigDirsHook resolves, in preference order, the directories that
// may hold the device/org config file (config.yaml: device_api_token,
// api_base_url, org_id, org_name, user_email, user_name, redis_url,
// aceteam_api_key): network.GetNodeConfigDir() (the machine-convergent node
// config dir, already used for identity.json/ssh_sync.yaml) first, then the
// legacy invoker-scoped platform.ConfigDir() as a read-only fallback for a
// node whose config was written before citadel-cli#845.
//
// internal/config is a leaf that must not import internal/network (see
// cmd/nodevault_hooks.go's identical config.VaultConfigured/VaultVerify
// pattern), so cmd wires this hook at init time -- every citadel entrypoint
// loads it via main -> cmd.Execute. A nil hook (e.g. a standalone test binary
// that never called cmd.Execute) makes LoadDeviceCredsConverged fall back to
// platform.ConfigDir() alone -- the pre-#845, leaf-safe behavior -- rather
// than panicking.
var DeviceConfigDirsHook func() []string

// LoadDeviceCredsConverged loads device creds by trying DeviceConfigDirsHook()
// (or, if unset, platform.ConfigDir() alone) in preference order and
// returning the first entry with a non-empty token. Prefer this over calling
// LoadDeviceCreds(platform.ConfigDir()) directly -- that single-directory
// form is invoker-scoped and will miss the config in a cross-context read
// (see DeviceConfigDirsHook).
func LoadDeviceCredsConverged() DeviceCreds {
	if DeviceConfigDirsHook == nil {
		return LoadDeviceCreds(platform.ConfigDir())
	}
	return loadDeviceCredsFromDirs(DeviceConfigDirsHook())
}

// loadDeviceCredsFromDirs is the pure core of LoadDeviceCredsConverged: given
// an explicit, ordered directory list, return the first entry with a
// non-empty token. Split out (mirroring cmd.readDeviceConfigFromDirs) so a
// test can pass fabricated directories instead of the real
// DeviceConfigDirsHook(), whose entries depend on env/HOME/filesystem state.
func loadDeviceCredsFromDirs(dirs []string) DeviceCreds {
	for _, dir := range dirs {
		if c := LoadDeviceCreds(dir); c.Token != "" {
			return c
		}
	}
	return DeviceCreds{}
}

// DeviceCreds carries the minimal device-auth material needed to call the
// AceTeam backend as this node: the bearer token and the API base URL.
type DeviceCreds struct {
	// Token is the device_api_token minted at device authorization.
	Token string `yaml:"device_api_token"`
	// APIBaseURL is the AceTeam API base (e.g. "https://aceteam.ai"). May be
	// empty in older configs; callers should fall back to their own default.
	APIBaseURL string `yaml:"api_base_url"`
	// FabricNodeID is the numeric AceTeam fabric/platform node ID (aceteam
	// #8139), when the backend has echoed one back to this node. Read from
	// the SAME config.yaml as Token/APIBaseURL. Empty on every node today --
	// see docs/design-node-identity-receipts.md §2 -- until a backend echo
	// point (device-auth /token response, or a heartbeat ack) starts sending
	// it. Included here (not just in cmd.DeviceConfig) so a leaf package like
	// internal/aep/internal/worker can resolve a signed receipt's node_id
	// without importing cmd.
	FabricNodeID string `yaml:"fabric_node_id"`
}

// LoadDeviceCreds reads the device bearer token + API base URL from
// {configDir}/config.yaml. A missing or unparseable file yields zero-valued
// creds (Token == ""), which callers treat as "not authenticated, skip".
// Reading at use-time (rather than caching at startup) means a token rotated by
// the worker's in-place reauth is picked up on the next call.
func LoadDeviceCreds(configDir string) DeviceCreds {
	data, err := os.ReadFile(filepath.Join(configDir, deviceCredsFile))
	if err != nil {
		return DeviceCreds{}
	}
	var c DeviceCreds
	if err := yaml.Unmarshal(data, &c); err != nil {
		return DeviceCreds{}
	}
	return c
}
