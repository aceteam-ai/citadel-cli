package teamchat

// Token sources, in resolution priority order. Mirrors the `citadel mcp`
// credential chain (flag/env > config > device token) so Team Chat and the
// MCP bridge behave consistently.
const (
	// TokenSourceEnv is the ACETEAM_API_KEY environment variable.
	TokenSourceEnv = "env"
	// TokenSourceConfig is the aceteam_api_key field in config.yaml.
	TokenSourceConfig = "config"
	// TokenSourceDevice is the node's device_api_token. It authenticates but
	// is scope-denied on /api/channels/** today (see citadel-cli#495), so the
	// UI warns when this is the only credential available.
	TokenSourceDevice = "device"
)

// ResolveToken picks the Team Chat credential from the available sources:
// the ACETEAM_API_KEY environment variable, then the aceteam_api_key config
// field, then the device token. Returns the token and its source label, or
// ("", "") when nothing is configured.
func ResolveToken(envKey, configKey, deviceToken string) (token, source string) {
	switch {
	case envKey != "":
		return envKey, TokenSourceEnv
	case configKey != "":
		return configKey, TokenSourceConfig
	case deviceToken != "":
		return deviceToken, TokenSourceDevice
	default:
		return "", ""
	}
}
