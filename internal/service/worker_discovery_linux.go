//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	uid := os.Geteuid()
	if pinned, ok := ctx.Value(workerManagerUIDKey{}).(int); ok {
		uid = pinned
	}
	command.Env = discoveryManagerEnvironment(len(args) > 0 && args[0] == "--user", uid)
	command.WaitDelay = 500 * time.Millisecond
	var output discoveryOutput
	command.Stdout, command.Stderr = &output, io.Discard
	return output.result(runWorkerDiscoveryCommand(command))
}

// A fixture runner inspects the actual command without starting a process.
var runWorkerDiscoveryCommand = func(command *exec.Cmd) error { return command.Run() }

// DiscoverActiveWorkers examines the system manager and the caller's user
// manager, including the existing invoking-user manager under sudo. It never changes a unit or falls back to a fixed service-name list.
// Backing files must agree with the loaded manager (NeedDaemonReload=no).
func DiscoverActiveWorkers(executable string) ([]ManagedWorker, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return discoverActiveWorkersForProcess(ctx, executable, captureSystemctl, filepath.EvalSymlinks, os.Geteuid(), os.Getenv("SUDO_UID"), func(uid int) (bool, error) {
		info, err := os.Stat("/run/user/" + strconv.Itoa(uid))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil || !info.IsDir() {
			return false, errors.New("sudo user manager presence could not be verified")
		}
		return true, nil
	})
}

func discoverActiveWorkers(ctx context.Context, executable string, capture func(context.Context, ...string) ([]byte, error), canonical func(string) (string, error)) ([]ManagedWorker, error) {
	return discoverActiveWorkersForUID(ctx, executable, capture, canonical, os.Geteuid())
}

type workerManagerUIDKey struct{}
type workerManagerScope struct {
	user     bool
	uid      int
	required bool
}

// The root user manager is optional: sudo does not start one. The invoking
// user's existing manager remains required so its worker cannot be hidden.
func discoverActiveWorkersForProcess(ctx context.Context, executable string, capture func(context.Context, ...string) ([]byte, error), canonical func(string) (string, error), euid int, sudoUID string, exists func(int) (bool, error)) ([]ManagedWorker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scopes := []workerManagerScope{{false, euid, true}, {true, euid, euid != 0}}
	if euid == 0 && sudoUID != "" {
		uid, err := strconv.Atoi(sudoUID)
		if err != nil || uid < 0 || strconv.Itoa(uid) != sudoUID {
			return nil, errors.New("sudo user manager identity could not be verified")
		}
		if uid != 0 {
			present, err := exists(uid)
			if err != nil {
				return nil, errors.New("service discovery incomplete: sudo user manager presence could not be verified")
			}
			if present {
				scopes = append(scopes, workerManagerScope{true, uid, true})
			}
		}
	}
	return discoverActiveWorkersInManagers(ctx, executable, capture, canonical, scopes)
}

// Retain the explicit two-manager fixture entrypoint; real process discovery
// uses the root/sudo-aware planner above, with its presence probe injected.
func discoverActiveWorkersForUID(ctx context.Context, executable string, capture func(context.Context, ...string) ([]byte, error), canonical func(string) (string, error), managerUID int) ([]ManagedWorker, error) {
	return discoverActiveWorkersInManagers(ctx, executable, capture, canonical, []workerManagerScope{{false, managerUID, true}, {true, managerUID, true}})
}

func discoverActiveWorkersInManagers(ctx context.Context, executable string, capture func(context.Context, ...string) ([]byte, error), canonical func(string) (string, error), scopes []workerManagerScope) ([]ManagedWorker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expected, err := canonical(executable)
	if err != nil {
		return nil, err
	}
	var workers []ManagedWorker
	managers := 0
	var unavailable []string
	for _, scope := range scopes {
		userMode, managerUID := scope.user, scope.uid
		managerCtx := context.WithValue(ctx, workerManagerUIDKey{}, managerUID)
		prefix := systemctlArgs(userMode)
		listArgs := append(append([]string{}, prefix...), "list-units", "--type=service", "--state=active", "--no-legend", "--no-pager", "--plain")
		listed, err := capture(managerCtx, listArgs...)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errWorkerDiscoveryBound) {
				return nil, errWorkerDiscoveryBound
			}
			manager := "system"
			if userMode {
				manager = "user"
			}
			if scope.required {
				unavailable = append(unavailable, manager)
			}
			continue
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
			content, err := capture(managerCtx, catArgs...)
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
			reload, err := capture(managerCtx, showArgs...)
			if err != nil || len(reload) > 16 || strings.TrimSpace(string(reload)) != "no" {
				return nil, errors.New("active service unit is not verified against the loaded manager")
			}
			path, account, known := workerUnitIdentity(string(content))
			if !known {
				// Unsupported syntax in an unrelated daemon cannot hide this
				// executable. A plausible worker still requires inspection.
				if unitCouldRunExecutable(string(content), executable, expected) {
					return nil, errors.New("active worker executable or account identity requires manual inspection")
				}
				continue
			}
			if path == "" {
				continue
			}
			resolved, err := canonical(path)
			if err != nil || resolved != expected {
				continue
			}
			if account == "" {
				if userMode {
					account = strconv.Itoa(managerUID)
				} else {
					account = "root"
				}
			}
			unit := ManagedUnit{Name: strings.TrimSuffix(name, ".service"), UserMode: userMode}
			if userMode {
				unit.ManagerUID = strconv.Itoa(managerUID)
			}
			workers = append(workers, ManagedWorker{Unit: unit, Executable: resolved, User: account, AutoUpdateDisabled: unitAutoUpdateDisabled(string(content))})
		}
	}
	if managers == 0 {
		return nil, errors.New("no service manager could be inspected")
	}
	if len(unavailable) != 0 {
		return nil, errors.New("service discovery incomplete: could not inspect " + strings.Join(unavailable, " and ") + " manager")
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
// filtering. Empty path with known=true is a verified non-worker; any
// known=false observation of a plausible worker refuses discovery. Empty ExecStart resets
// the list, including across drop-ins.
func workerUnitIdentity(content string) (path, account string, known bool) {
	section := ""
	var commands []string
	dynamicUser := ""
	mappings := map[string]bool{}
	lines, complete := unitLogicalLines(content)
	if !complete {
		return "", "", false
	}
	for _, raw := range lines {
		line := strings.Trim(raw, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
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
	if len(commands) == 0 {
		return "", "", true
	}
	if len(commands) != 1 {
		return "", "", false
	}
	words, privileged, ok := unitCommandWords(commands[0])
	if !ok || len(words) == 0 {
		return "", "", false
	}
	path = words[0]
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "%$\x00\r\n\t") {
		return "", "", false
	}
	if len(words) == 1 {
		return "", "", true
	}
	workIndex := 1
	if len(words) >= 3 && (words[1] == "--no-auto-update" || strings.HasPrefix(words[1], "--no-auto-update=")) {
		workIndex = 2
	}
	if words[workIndex] == "work" {
		if privileged && account != "" {
			return path, "", false
		}
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
	for _, word := range words[1:] {
		if word == "work" {
			return path, account, false
		}
	}
	switch filepath.Base(path) {
	case "sh", "bash", "dash", "zsh", "fish", "env":
		return "", "", false
	}
	return "", "", true
}

// The inspected user bus is pinned to the same effective UID carried on each
// observed user unit. Do not inherit a different user's bus address from sudo.
func discoveryManagerEnvironment(userMode bool, uid int) []string {
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	if userMode {
		runtimeDir := "/run/user/" + strconv.Itoa(uid)
		environment = append(environment, "XDG_RUNTIME_DIR="+runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus")
	}
	return environment
}

// Only literal unit policy is evidence. Never read EnvironmentFile contents or
// infer policy from this CLI's own environment. Unset/reset precedence matters.
func unitAutoUpdateDisabled(content string) bool {
	section, disabled, unknownFile := "", false, false
	var commands, unsetRules []string
	unknownUnset := false
	lines, complete := unitLogicalLines(content)
	if !complete {
		return false
	}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section != "Service" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "EnvironmentFile":
			unknownFile = value != ""
		case "Environment":
			if value == "" {
				disabled = false
				continue
			}
			words, known := literalUnitWords(value)
			if !known {
				disabled = false
				continue
			}
			for _, word := range words {
				name, setting, assigned := strings.Cut(word, "=")
				if assigned && name == "CITADEL_NO_AUTO_UPDATE" {
					disabled = setting == "1"
				}
			}
		case "UnsetEnvironment":
			if value == "" {
				unsetRules, unknownUnset = nil, false
				continue
			}
			words, known := literalUnitWords(value)
			if !known {
				unknownUnset = true
			}
			unsetRules = append(unsetRules, words...)
		case "ExecStart":
			if value == "" {
				commands = nil
			} else {
				commands = append(commands, value)
			}
		}
	}
	if len(commands) == 1 {
		words, _, known := unitCommandWords(commands[0])
		if known {
			for index, word := range words {
				if word == "--" {
					break
				}
				if index == 1 || index == 2 && words[1] == "work" {
					if word == "--no-auto-update" {
						return true
					}
					if strings.HasPrefix(word, "--no-auto-update=") {
						flag, err := strconv.ParseBool(strings.TrimPrefix(word, "--no-auto-update="))
						if err == nil && flag {
							return true
						}
					}
				}
			}
		}
	}
	for _, rule := range unsetRules {
		if rule == "CITADEL_NO_AUTO_UPDATE" || rule == "CITADEL_NO_AUTO_UPDATE=1" {
			disabled = false
		}
	}
	return disabled && !unknownFile && !unknownUnset
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

// systemd folds a continuation into one space and skips intervening comments.
// Preserve unsupported escapes for the literal parser to refuse, not evaluate.
func unitLogicalLines(content string) ([]string, bool) {
	var lines []string
	pending := ""
	continuing := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		// Blank lines and systemctl cat file headers end a logical line; an
		// ordinary intervening comment may still be skipped within a continuation.
		if line == "" || strings.HasPrefix(line, "# /") {
			if continuing {
				lines = append(lines, pending)
				pending, continuing = "", false
			}
			if line == "" {
				lines = append(lines, "")
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		slashes := len(line) - len(strings.TrimRight(line, "\\"))
		if slashes%2 == 1 {
			pending += strings.TrimSuffix(line, "\\") + " "
			continuing = true
			continue
		}
		lines = append(lines, pending+line)
		pending, continuing = "", false
	}
	return lines, !continuing
}

// Prefixes modify execution, not the executable path. With @ the next word
// supplies argv[0], so the subcommand follows that word.
func unitCommandWords(command string) ([]string, bool, bool) {
	words, ok := literalUnitWords(command)
	if !ok || len(words) == 0 {
		return nil, false, false
	}
	prefixEnd := 0
	for prefixEnd < len(words[0]) && strings.ContainsRune("-@:+!", rune(words[0][prefixEnd])) {
		prefixEnd++
	}
	prefix := words[0][:prefixEnd]
	words[0] = words[0][prefixEnd:]
	if words[0] == "" {
		return nil, false, false
	}
	if strings.Contains(prefix, "@") {
		if len(words) < 2 {
			return nil, false, false
		}
		words = append(words[:1], words[2:]...)
	}
	return words, strings.ContainsAny(prefix, "+!"), true
}

func unitCouldRunExecutable(content string, paths ...string) bool {
	for _, path := range paths {
		base := filepath.Base(path)
		if path != "" && (strings.Contains(content, path) || base != "." && strings.Contains(content, base)) {
			return true
		}
	}
	return false
}
