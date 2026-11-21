package containersession

import (
	"gopkg.in/yaml.v3"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

// renderedDeclaration is the single-session config the in-container holder
// loads. It is a separate struct so empty optional fields are omitted: the
// strict decoder rejects an empty command_restart, and a container declaration
// may not carry host-only fields at all.
type renderedDeclaration struct {
	Name            string                           `yaml:"name"`
	Harness         string                           `yaml:"harness"`
	Mode            string                           `yaml:"mode"`
	Command         []string                         `yaml:"command"`
	Cwd             string                           `yaml:"cwd"`
	Env             map[string]string                `yaml:"env,omitempty"`
	EnvFile         string                           `yaml:"env_file,omitempty"`
	Restart         declaredsession.RestartPolicy    `yaml:"restart"`
	Attach          declaredsession.AttachPolicy     `yaml:"attach"`
	ScrollbackBytes int                              `yaml:"scrollback_bytes"`
	Log             declaredsession.LogPolicy        `yaml:"log"`
	AceTeam         declaredsession.AceTeamPolicy    `yaml:"aceteam"`
	Enabled         bool                             `yaml:"enabled"`
	Runtime         *declaredsession.RuntimeSpec     `yaml:"runtime"`
	Credentials     []declaredsession.CredentialSpec `yaml:"credentials,omitempty"`
}

// RenderConfig returns the sessions.yaml text the container's holder loads: the
// declaration with the node-rendered resume-aware launcher as its command, and
// the credential env file attached when any credential is an env variable.
func RenderConfig(d declaredsession.Declaration) ([]byte, error) {
	r := renderedDeclaration{
		Name: d.Name, Harness: d.Harness, Mode: d.Mode, Command: LaunchCommand(d), Cwd: d.Cwd,
		Env: d.Env, Restart: d.Restart, Attach: d.Attach, ScrollbackBytes: d.ScrollbackBytes,
		Log: d.Log, AceTeam: d.AceTeam, Enabled: d.Enabled, Runtime: d.Runtime, Credentials: d.Credentials,
	}
	for _, c := range d.Credentials {
		if c.Env != "" {
			r.EnvFile = declaredsession.ContainerCredEnvFile
		}
	}
	return yaml.Marshal(map[string][]renderedDeclaration{"sessions": {r}})
}
