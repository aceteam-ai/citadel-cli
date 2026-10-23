// Package declaredsession contains the node-local S1 declaration and storage
// boundary. It neither launches processes nor reads device credentials. Config
// edits are never written back by this package; clean exits use a durable overlay.
package declaredsession

import (
	"errors"
	"time"
)

var (
	ErrConfig      = errors.New("declared session configuration refused")
	ErrUnsafe      = errors.New("declared session storage refused")
	ErrUnavailable = errors.New("declared session storage unavailable")
	ErrConflict    = errors.New("declared session configuration conflict")
	ErrBusy        = errors.New("declared session operation busy")
	ErrUnsupported = errors.New("declared sessions require Linux")
)

type RestartPolicy struct {
	Policy     string `yaml:"policy" json:"policy"`
	MinSeconds int    `yaml:"min_s" json:"min_s"`
	MaxSeconds int    `yaml:"max_s" json:"max_s"`
	ResetAfter int    `yaml:"reset_after_s" json:"reset_after_s"`
}

type AttachPolicy struct {
	Local      string `yaml:"local" json:"local"`
	Web        string `yaml:"web" json:"web"`
	MaxViewers int    `yaml:"max_viewers" json:"max_viewers"`
}

type LogPolicy struct {
	RotateBytes int64 `yaml:"rotate_bytes" json:"rotate_bytes"`
	Keep        int   `yaml:"keep" json:"keep"`
}

type AceTeamPolicy struct {
	Register bool `yaml:"register" json:"register"`
	Takeover bool `yaml:"takeover" json:"takeover"`
}

type Declaration struct {
	Name            string            `yaml:"name" json:"name"`
	Harness         string            `yaml:"harness" json:"harness"`
	Mode            string            `yaml:"mode" json:"mode"`
	Command         []string          `yaml:"command" json:"command"`
	CommandRestart  []string          `yaml:"command_restart" json:"command_restart"`
	Cwd             string            `yaml:"cwd" json:"cwd"`
	PathPrepend     []string          `yaml:"path_prepend" json:"path_prepend"`
	Env             map[string]string `yaml:"env" json:"env"`
	EnvFile         string            `yaml:"env_file" json:"env_file"`
	Restart         RestartPolicy     `yaml:"restart" json:"restart"`
	Attach          AttachPolicy      `yaml:"attach" json:"attach"`
	ScrollbackBytes int               `yaml:"scrollback_bytes" json:"scrollback_bytes"`
	Log             LogPolicy         `yaml:"log" json:"log"`
	AceTeam         AceTeamPolicy     `yaml:"aceteam" json:"aceteam"`
	Enabled         bool              `yaml:"enabled" json:"enabled"`
	// Revision binds one normalized declaration, not unrelated YAML edits.
	Revision string `yaml:"-" json:"-"`
}

type Snapshot struct {
	FileRevision string
	Sessions     []Declaration
}

func (s Snapshot) Lookup(name string) (Declaration, bool) {
	for _, d := range s.Sessions {
		if d.Name == name {
			return d, true
		}
	}
	return Declaration{}, false
}

// Process and scope observations are not authority. The manager must compare
// authenticated peer credentials and live process/scope identity before control.
type ProcessIdentity struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
	UID        uint32 `json:"uid"`
	BootID     string `json:"boot_id"`
}

type ScopeIdentity struct {
	Name         string `json:"name"`
	Manager      string `json:"manager"`
	InvocationID string `json:"invocation_id"`
	ControlGroup string `json:"control_group"`
}

type Exit struct {
	Code            int       `json:"code"`
	Signal          int       `json:"signal"`
	ObservedAt      time.Time `json:"observed_at"`
	ChildGeneration uint64    `json:"child_generation"`
}

type State struct {
	Version             int             `json:"version"`
	Name                string          `json:"name"`
	Incarnation         string          `json:"incarnation"`
	Holder              ProcessIdentity `json:"holder"`
	Child               ProcessIdentity `json:"child"`
	Scope               ScopeIdentity   `json:"scope"`
	ProtocolVersion     int             `json:"protocol_version"`
	BinaryVersion       string          `json:"binary_version"`
	StartedAt           time.Time       `json:"started_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	RestartCount        uint64          `json:"restart_count"`
	ChildGeneration     uint64          `json:"child_generation"`
	LastExit            *Exit           `json:"last_exit,omitempty"`
	FileRevision        string          `json:"file_revision"`
	DeclarationRevision string          `json:"declaration_revision"`
	Phase               string          `json:"phase"`
	Guarantee           string          `json:"guarantee"`
	ResolvedExecutable  string          `json:"resolved_executable,omitempty"`
	EnvironmentHash     string          `json:"environment_hash,omitempty"`
}

type StopMarker struct {
	Version             int       `json:"version"`
	Name                string    `json:"name"`
	DeclarationRevision string    `json:"declaration_revision"`
	Incarnation         string    `json:"incarnation"`
	StoppedAt           time.Time `json:"stopped_at"`
}

// Event intentionally has no free-form detail, argv, environment value or
// console field. Kind/Reason are finite code-owned vocabularies.
type Event struct {
	Kind               string    `json:"kind"`
	At                 time.Time `json:"at"`
	Incarnation        string    `json:"incarnation"`
	ChildGeneration    uint64    `json:"child_generation,omitempty"`
	ViewerID           string    `json:"viewer_id,omitempty"`
	Role               string    `json:"role,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	ResolvedExecutable string    `json:"resolved_executable,omitempty"`
	EnvironmentHash    string    `json:"environment_hash,omitempty"`
}

type Paths struct {
	Name          string
	Dir           string
	Socket        string
	State         string
	OperationLock string
	OwnerLock     string
}
