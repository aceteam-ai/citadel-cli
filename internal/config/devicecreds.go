package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// deviceCredsFile is the global device-auth config written by `citadel init` /
// device authorization. It is the SAME file cmd.getDeviceConfigFromFile reads;
// this loader deliberately decodes only the two fields a background handler
// needs (the device API token + the API base URL) so a handler in internal/
// can authenticate against the AceTeam backend without importing cmd.
const deviceCredsFile = "config.yaml"

// DeviceCreds carries the minimal device-auth material needed to call the
// AceTeam backend as this node: the bearer token and the API base URL.
type DeviceCreds struct {
	// Token is the device_api_token minted at device authorization.
	Token string `yaml:"device_api_token"`
	// APIBaseURL is the AceTeam API base (e.g. "https://aceteam.ai"). May be
	// empty in older configs; callers should fall back to their own default.
	APIBaseURL string `yaml:"api_base_url"`
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
