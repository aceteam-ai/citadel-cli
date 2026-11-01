// Package memorygit implements the inactive MEMORY_GIT_V1 read-only contract.
// It is deliberately not registered with any worker or capability publisher.
package memorygit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	JobType         = "MEMORY_GIT_V1"
	MaxRequestBytes = 4096
	MaxOutputBytes  = 256 * 1024
	MaxHistory      = 100
)

type Operation string

const (
	Resolve Operation = "resolve"
	History Operation = "history"
	Diff    Operation = "diff"
	Export  Operation = "export"
)

// Request contains operands, never a repository path, executable, argv or shell.
// Scope is an opaque key into a node-owned mapping, not a filesystem path.
type Request struct {
	Version   int       `json:"version"`
	NodeID    string    `json:"node_id"`
	Scope     string    `json:"scope"`
	Operation Operation `json:"operation"`
	Name      string    `json:"name,omitempty"`
	Revision  string    `json:"revision,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Limit     int       `json:"limit,omitempty"`
}

type Commit struct {
	SHA     string `json:"sha"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// Result never exposes stderr or paths. Export is byte-preserving base64;
// over-limit output fails atomically instead of returning truncated content.
type Result struct {
	Version       int       `json:"version"`
	Operation     Operation `json:"operation"`
	SHA           string    `json:"sha,omitempty"`
	FromSHA       string    `json:"from_sha,omitempty"`
	ToSHA         string    `json:"to_sha,omitempty"`
	Commits       []Commit  `json:"commits,omitempty"`
	Diff          string    `json:"diff,omitempty"`
	ContentBase64 string    `json:"content_base64,omitempty"`
	Bytes         int       `json:"bytes"`
}

type Error struct {
	Code string `json:"reason"`
}

func (e *Error) Error() string  { encoded, _ := json.Marshal(e); return string(encoded) }
func failure(code string) error { return &Error{Code: code} }

var (
	shaRE       = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	headRE      = regexp.MustCompile(`^HEAD(~[1-9][0-9]?)?$`)
	slugRE      = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,251}$`)
	scopeRE     = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,127}$`)
	directoryRE = regexp.MustCompile(`^memories/[a-zA-Z0-9_-]{1,255}$`)
)

func validRevision(s string) bool { return shaRE.MatchString(s) || headRE.MatchString(s) }

// Validate runs before opening the repository or starting a child process.
// Operands for a different operation are rejected, never silently ignored.
func (r Request) Validate(nodeID string) error {
	if nodeID == "" || r.NodeID != nodeID {
		return failure("node_mismatch")
	}
	if r.Version != 1 || !scopeRE.MatchString(r.Scope) {
		return failure("invalid_request")
	}
	if r.Operation != Resolve && !slugRE.MatchString(r.Name) {
		return failure("invalid_request")
	}
	switch r.Operation {
	case Resolve:
		if r.Name != "" || !validRevision(r.Revision) || r.From != "" || r.To != "" || r.Limit != 0 {
			return failure("invalid_request")
		}
	case History:
		if r.Revision != "" || r.From != "" || r.To != "" || r.Limit < 1 || r.Limit > MaxHistory {
			return failure("invalid_request")
		}
	case Diff:
		if !validRevision(r.From) || !validRevision(r.To) || r.Revision != "" || r.Limit != 0 {
			return failure("invalid_request")
		}
	case Export:
		if !validRevision(r.Revision) || r.From != "" || r.To != "" || r.Limit != 0 {
			return failure("invalid_request")
		}
	default:
		return failure("invalid_request")
	}
	return nil
}

// Engine configuration belongs to the node. Scopes must be independently
// authorized by the platform before dispatch. Do not map org scopes onto a
// personal directory. New copies the mapping so callers cannot mutate it.
type Engine struct {
	nodeID  string
	repoDir string
	gitPath string
	scopes  map[string]string
}

func New(nodeID, repoDir, gitPath string, scopes map[string]string) (*Engine, error) {
	if nodeID == "" || !validAbsolutePath(repoDir) || !validAbsolutePath(gitPath) || len(scopes) == 0 {
		return nil, failure("invalid_configuration")
	}
	copyScopes := make(map[string]string, len(scopes))
	for scope, directory := range scopes {
		if !scopeRE.MatchString(scope) || !directoryRE.MatchString(directory) {
			return nil, failure("invalid_configuration")
		}
		copyScopes[scope] = directory
	}
	return &Engine{nodeID: nodeID, repoDir: repoDir, gitPath: gitPath, scopes: copyScopes}, nil
}

func (e *Engine) Execute(ctx context.Context, r Request) (Result, error) {
	if err := r.Validate(e.nodeID); err != nil {
		return Result{}, err
	}
	directory, ok := e.scopes[r.Scope]
	if !ok {
		return Result{}, failure("scope_denied")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	repo, err := openRepository(ctx, e.repoDir)
	if err != nil {
		return Result{}, err
	}
	defer repo.Close()
	run := func(args ...string) ([]byte, error) { return runGit(ctx, e.gitPath, repo, args...) }
	resolve := func(revision string) (string, error) {
		out, err := run("rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
		if err != nil {
			return "", err
		}
		sha := strings.TrimSpace(string(out))
		if len(sha) != 40 || !shaRE.MatchString(sha) {
			return "", failure("invalid_git_result")
		}
		return sha, nil
	}
	result := Result{Version: 1, Operation: r.Operation}
	path := directory + "/" + r.Name + ".md"
	switch r.Operation {
	case Resolve:
		result.SHA, err = resolve(r.Revision)
	case History:
		// Pin HEAD before the log so one request observes one commit snapshot.
		result.SHA, err = resolve("HEAD")
		if err != nil {
			break
		}
		var out []byte
		out, err = run("log", "--no-show-signature", "--format=%H%x00%aI%x00%s%x00", "-n", strconv.Itoa(r.Limit), result.SHA, "--", path)
		if err != nil {
			break
		}
		result.Bytes = len(out)
		for _, record := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\x00\n") {
			if record == "" {
				continue
			}
			fields := strings.Split(strings.TrimSuffix(record, "\x00"), "\x00")
			if len(fields) != 3 || len(fields[0]) != 40 || !shaRE.MatchString(fields[0]) {
				return Result{}, failure("invalid_git_result")
			}
			result.Commits = append(result.Commits, Commit{SHA: fields[0], Date: fields[1], Subject: fields[2]})
		}
	case Diff:
		result.FromSHA, err = resolve(r.From)
		if err != nil {
			break
		}
		result.ToSHA, err = resolve(r.To)
		if err != nil {
			break
		}
		var out []byte
		out, err = run("diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--unified=3", result.FromSHA, result.ToSHA, "--", path)
		result.Diff, result.Bytes = string(out), len(out)
	case Export:
		result.SHA, err = resolve(r.Revision)
		if err != nil {
			break
		}
		// ls-tree proves this is a regular tracked blob. A symlink's target
		// bytes or submodule must never be treated as an exported memory.
		var entry []byte
		entry, err = run("ls-tree", "-z", result.SHA, "--", path)
		if err != nil {
			break
		}
		parts := strings.SplitN(string(entry), "\t", 2)
		if len(parts) != 2 || parts[1] != path+"\x00" {
			return Result{}, failure("file_unavailable")
		}
		metadata := strings.Fields(parts[0])
		if len(metadata) != 3 || (metadata[0] != "100644" && metadata[0] != "100755") || metadata[1] != "blob" || len(metadata[2]) != 40 || !shaRE.MatchString(metadata[2]) {
			return Result{}, failure("file_unavailable")
		}
		var out []byte
		out, err = run("cat-file", "blob", metadata[2])
		result.ContentBase64, result.Bytes = base64.StdEncoding.EncodeToString(out), len(out)
	}
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}
