package declaredsession

import (
	"path"
	"strings"
)

const (
	maxCredentials = 16

	// ContainerHome and ContainerWorkRoot are fixed in-container paths. Both
	// harnesses key their transcripts on the working directory, so a fixed path
	// is what lets a transcript resume on any node.
	ContainerHome     = "/home/agent"
	ContainerWorkRoot = "/home/agent/work"
	// ContainerNodeDir is where the holder sees its own node config dir.
	ContainerNodeDir = "/run/session"
	// ContainerCredDir is the read-only tmpfs-backed directory holding the
	// credential env file; a whole directory is bound so the engine never has to
	// create a mountpoint stub in a host directory.
	ContainerCredDir = "/run/credentials"

	RuntimeContainer = "container"

	// ContainerCredEnvFile is the only env_file a container declaration may name:
	// the node materializes credential environment variables there, on tmpfs,
	// at wake. Its contents are never part of the declaration revision.
	ContainerCredEnvFile = ContainerCredDir + "/credentials.env"
)

// ContainerImagePrefix is the only registry a session image may come from,
// matching the BYOC allowlist. Fail-closed: an operator pins a vetted image.
const ContainerImagePrefix = "ghcr.io/aceteam-ai/"

// HarnessConfig describes where a harness keeps its state under ContainerHome.
// Everything in Dir is backed by host tmpfs EXCEPT the Persist subpaths, which
// are bound from the named volume on top. That split is what keeps credentials
// (and the temp files a rename-based writer stages beside them) off the volume
// while transcripts survive. Pinned from the vendors' docs:
//
//   - Claude Code (code.claude.com/docs/en/claude-directory): transcripts and
//     auto memory live under projects/, checkpoints in file-history/, plans in
//     plans/, tasks in tasks/, subagent memory in agent-memory/; the login is
//     .credentials.json at the top of the directory.
//   - Codex: auth.json at the top of CODEX_HOME; session rollouts under
//     sessions/YYYY/MM/DD (stated in the #1165 design pass; the public config
//     page does not name them), memories/, rules/ and skills/.
//
// Single files (history.jsonl, settings.json, config.toml) stay on tmpfs: a
// file cannot be bound from a volume subpath without being created first, and
// losing prompt recall at sleep is acceptable. Re-pin if a vendor moves a path.
type HarnessConfig struct {
	Dir     string
	Persist []string
}

// HarnessConfigFor returns the pinned layout, or false for an unsupported harness.
func HarnessConfigFor(harness string) (HarnessConfig, bool) {
	switch harness {
	case "claude-code":
		return HarnessConfig{Dir: ContainerHome + "/.claude", Persist: []string{"projects", "file-history", "plans", "tasks", "agent-memory", "skills", "commands", "agents"}}, true
	case "codex":
		return HarnessConfig{Dir: ContainerHome + "/.codex", Persist: []string{"sessions", "memories", "rules", "skills"}}, true
	}
	return HarnessConfig{}, false
}

// IsContainer reports whether the declaration selects the container backend.
func (d Declaration) IsContainer() bool {
	return d.Runtime != nil && d.Runtime.Kind == RuntimeContainer
}

// validateContainerDeclaration owns what a container declaration may say. A
// host-process declaration (Runtime == nil) must not carry container-only
// fields, so a typo can never half-select the container backend.
func validateContainerDeclaration(d Declaration) error {
	if d.Runtime == nil {
		if len(d.Credentials) != 0 {
			return ErrConfig
		}
		return nil
	}
	r := d.Runtime
	if r.Kind != RuntimeContainer || d.Mode != "interactive" || !oneOf(d.Harness, "claude-code", "codex") {
		return ErrConfig
	}
	if strings.ContainsAny(d.Cwd, ":,\"") {
		return ErrConfig
	}
	hc, _ := HarnessConfigFor(d.Harness)
	if !strings.HasPrefix(r.Image, ContainerImagePrefix) || len(r.Image) > 512 || strings.ContainsAny(r.Image, " \t\n\x00") {
		return ErrConfig
	}
	if !oneOf(r.Size, "", "nano", "small", "medium", "large") || !oneOf(r.OnBoot, "", "resume", "asleep") {
		return ErrConfig
	}
	clean := path.Clean(d.Cwd)
	if clean != ContainerWorkRoot && !strings.HasPrefix(clean, ContainerWorkRoot+"/") {
		return ErrConfig
	}
	// The node owns session identity and resume; a declared flag must not
	// override it (-c/--continue picks "most recent", never "this session").
	// The node-rendered launcher is /bin/sh -c <script>; its "-c" is not a
	// harness flag.
	if d.Command[0] != "/bin/sh" {
		for _, arg := range d.Command[1:] {
			switch arg {
			case "-c", "--continue", "-r", "--resume", "--session-id", "resume":
				return ErrConfig
			}
		}
	}
	if (d.EnvFile != "" && d.EnvFile != ContainerCredEnvFile) || len(d.PathPrepend) != 0 || d.CommandRestart != nil {
		return ErrConfig // host-path concepts have no meaning inside the container
	}
	seen := map[string]bool{}
	for _, c := range d.Credentials {
		if ValidateName(c.Name) != nil || seen[c.Name] {
			return ErrConfig
		}
		seen[c.Name] = true
		if (c.Path == "") == (c.Env == "") {
			return ErrConfig
		}
		if c.Path != "" {
			p := path.Clean(c.Path)
			// A credential file must sit inside the harness's tmpfs-backed config
			// dir and outside every persisted (volume) subpath.
			if !path.IsAbs(c.Path) || p != c.Path || !strings.HasPrefix(p, hc.Dir+"/") || strings.ContainsAny(p, ":,\"") {
				return ErrConfig
			}
			rel := strings.TrimPrefix(p, hc.Dir+"/")
			for _, sub := range hc.Persist {
				if rel == sub || strings.HasPrefix(rel, sub+"/") {
					return ErrConfig
				}
			}
		}
		if c.Env != "" && (!envPattern.MatchString(c.Env) || reservedEnv(c.Env)) {
			return ErrConfig
		}
	}
	return nil
}
