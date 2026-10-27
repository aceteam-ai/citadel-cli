//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxWorkerDiscoveryOutput = 256 << 10
const maxWorkerDiscoveryUnits = 512

var errWorkerDiscoveryBound = errors.New("service discovery output exceeded its bound")

// Keep the buffer named: embedding it promotes ReadFrom, which lets io.Copy
// bypass Write's bound when os/exec copies a command's stdout pipe.
type discoveryOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *discoveryOutput) Len() int      { return b.buffer.Len() }
func (b *discoveryOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *discoveryOutput) Write(data []byte) (int, error) {
	if b.exceeded || len(data) > maxWorkerDiscoveryOutput-b.Len() {
		b.exceeded = true
		return 0, errWorkerDiscoveryBound
	}
	return b.buffer.Write(data)
}

func (b *discoveryOutput) result(runErr error) ([]byte, error) {
	// Cmd.Wait can prefer the child's exit error over a stdout copy error.
	// Keep overflow as an independent observation, never an unavailable-manager
	// error that would let discovery use an incomplete set of workers.
	if b.exceeded {
		return nil, errWorkerDiscoveryBound
	}
	if runErr != nil {
		return nil, runErr
	}
	return b.Bytes(), nil
}

func captureSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "systemctl", args...)
	command.WaitDelay = 500 * time.Millisecond
	var output discoveryOutput
	command.Stdout, command.Stderr = &output, io.Discard
	return output.result(command.Run())
}

// DiscoverActiveWorkers examines the system manager and the caller's user
// manager. It never changes a unit or falls back to a fixed service-name list.
// Backing files must agree with the loaded manager (NeedDaemonReload=no).
func DiscoverActiveWorkers(executable string) ([]ManagedWorker, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return discoverActiveWorkers(ctx, executable, captureSystemctl, filepath.EvalSymlinks)
}

func discoverActiveWorkers(ctx context.Context, executable string, capture func(context.Context, ...string) ([]byte, error), canonical func(string) (string, error)) ([]ManagedWorker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expected, err := canonical(executable)
	if err != nil {
		return nil, err
	}
	var workers []ManagedWorker
	managers := 0
	for _, userMode := range []bool{false, true} {
		prefix := systemctlArgs(userMode)
		listArgs := append(append([]string{}, prefix...), "list-units", "--type=service", "--state=active", "--no-legend", "--no-pager", "--plain")
		listed, err := capture(ctx, listArgs...)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errWorkerDiscoveryBound) {
				return nil, errWorkerDiscoveryBound
			}
			continue // A host need not have both a system and a user manager.
		}
		managers++
		if len(listed) > maxWorkerDiscoveryOutput {
			return nil, errWorkerDiscoveryBound
		}
		names := map[string]struct{}{}
		for _, line := range strings.Split(string(listed), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || !safeServiceName(fields[0]) || fields[1] != "loaded" || fields[2] != "active" || fields[3] != "running" {
				continue
			}
			names[fields[0]] = struct{}{}
			if len(names) > maxWorkerDiscoveryUnits {
				return nil, errWorkerDiscoveryBound
			}
		}
		ordered := make([]string, 0, len(names))
		for name := range names {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		for _, name := range ordered {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			catArgs := append(append([]string{}, prefix...), "cat", "--no-pager", "--", name)
			content, err := capture(ctx, catArgs...)
			if err != nil {
				return nil, errors.New("could not inspect an active service unit")
			}
			if len(content) > maxWorkerDiscoveryOutput {
				return nil, errWorkerDiscoveryBound
			}
			// cat reads backing files, not the manager's loaded configuration.
			// A stale nonmatching file could hide another loaded worker, so this
			// check must precede exclusion as well as selection of a candidate.
			showArgs := append(append([]string{}, prefix...), "show", "--no-pager", "--property=NeedDaemonReload", "--value", "--", name)
			reload, err := capture(ctx, showArgs...)
			if err != nil || len(reload) > 16 || strings.TrimSpace(string(reload)) != "no" {
				return nil, errors.New("active service unit is not verified against the loaded manager")
			}
			path, account, known := workerUnitIdentity(string(content))
			if path == "" {
				continue
			}
			if !known {
				return nil, errors.New("active worker executable or account identity requires manual inspection")
			}
			resolved, err := canonical(path)
			if err != nil || resolved != expected {
				continue
			}
			if account == "" && !userMode {
				account = "root"
			}
			workers = append(workers, ManagedWorker{Unit: ManagedUnit{Name: strings.TrimSuffix(name, ".service"), UserMode: userMode}, Executable: resolved, User: account})
		}
	}
	if managers == 0 {
		return nil, errors.New("no service manager could be inspected")
	}
	return workers, nil
}

func safeServiceName(name string) bool {
	if len(name) > 255 || !strings.HasSuffix(name, ".service") || strings.HasPrefix(name, "-") || name == ".service" {
		return false
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("_.@:-", ch) {
			continue
		}
		return false
	}
	return true
}

// This accepts a literal command and common quoting, not shell evaluation or
// systemd specifier/environment expansion. Unknown syntax is left for manual
// inspection. A nonempty path with known=false is a literal worker whose
// account or filesystem mapping cannot be verified; it must not be hidden by
// filtering. Empty ExecStart resets the list, including across drop-ins.
func workerUnitIdentity(content string) (path, account string, known bool) {
	section := ""
	var commands []string
	dynamicUser := ""
	mappings := map[string]bool{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.Trim(raw, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			return "", "", false
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section != "Service" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", "", false
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "ExecStart":
			if value == "" {
				commands = nil
			} else {
				commands = append(commands, value)
			}
		case "User":
			account = value
		case "DynamicUser":
			dynamicUser = value
			if value == "" {
				// Unlike paths/lists, this boolean has no empty reset.
				dynamicUser = "unverified"
			}
		case "RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories":
			// Do not map a service namespace's literal path onto the host's
			// path. Empty assignments clear scalar and list mappings alike.
			mappings[key] = value != ""
		case "PrivateTmp", "PrivateDevices":
			// Empty boolean/enum assignments are invalid, not path resets.
			// Refuse rather than erase a previously enabled namespace.
			mappings[key] = value != "no" && value != "false" && value != "off" && value != "0"
		case "ProtectHome":
			// read-only preserves the host files, unlike yes or tmpfs.
			mappings[key] = value != "read-only" && value != "no" && value != "false" && value != "off" && value != "0"
		}
	}
	if len(commands) != 1 {
		return "", "", false
	}
	words, ok := literalUnitWords(commands[0])
	if !ok || len(words) < 2 {
		return "", "", false
	}
	path = strings.TrimPrefix(words[0], "-")
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "%$\x00\r\n\t") {
		return "", "", false
	}
	if words[1] == "work" || len(words) >= 3 && words[1] == "--no-auto-update" && words[2] == "work" {
		if strings.ContainsAny(account, "\"'\\%$ \t") || dynamicUser != "" && dynamicUser != "no" && dynamicUser != "false" && dynamicUser != "off" && dynamicUser != "0" {
			return path, "", false
		}
		for _, mapped := range mappings {
			if mapped {
				return path, account, false
			}
		}
		return path, account, true
	}
	return "", "", false
}

func literalUnitWords(line string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '\\' {
			if i+1 >= len(line) || !strings.ContainsRune("\\\"'", rune(line[i+1])) {
				return nil, false
			}
			i++
			word.WriteByte(line[i])
			started = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteByte(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			started = true
			continue
		}
		if ch == ' ' || ch == '\t' {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		if ch < 32 || ch == 127 {
			return nil, false
		}
		word.WriteByte(ch)
		started = true
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, true
}
