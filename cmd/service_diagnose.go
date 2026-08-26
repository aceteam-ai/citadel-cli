// cmd/service_diagnose.go
//
// `citadel service diagnose <name>` (citadel #852) is a single command that
// explains why a managed service is down or unhealthy: container state +
// exit code, the salient error line from its log tail, the effective compose
// command + resolved env (secrets redacted), a VRAM-fit check reusing #833's
// free-VRAM signal, and known-pattern hints (trust_remote_code, CUDA OOM,
// port conflicts, ...).
//
// Motivated by a real 2026-08-25 incident: diagnosing a stuck citadel-vllm
// took 3x manual `docker logs` + grepping the compose command + a manual
// `nvidia-smi` to find three separate root causes (a missing
// --trust-remote-code, an embedding model served with a chat command, and
// ~6.3GB free VRAM vs a ~16GB need). This wires the SAME building blocks
// 'citadel doctor' (#798) and 'citadel services' (#416) already use --
// manifest/compose resolution (cmd/service.go, cmd/logs.go), #833's
// free-VRAM signal (internal/status) -- into one command, and keeps the
// decision logic in internal/diagnose so it stays pure and unit-testable
// without docker (see that package's doc comment).
//
// Read-only, deliberately: this command never starts, stops, or modifies
// anything. Every gathering step below degrades to "unknown"/absent rather
// than failing the whole report when its input isn't available (docker
// missing, service unmanaged, no GPU, no manifest, ...).
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/diagnose"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/spf13/cobra"
)

var (
	diagnoseJSON   bool
	diagnoseNeedMB int
	diagnoseTail   int
)

var serviceDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <name>",
	Short: "Explain why a managed service is down or unhealthy",
	Long: `citadel service diagnose <name> gathers, in one command, what previously
took several manual steps to diagnose a down/unhealthy service:

  - Container state and exit code, plus the salient error line from the tail
    of its log (a Python traceback's final Error/Exception line, or the last
    non-empty log line).
  - The effective compose command and the resolved value of every ${VAR...}
    the compose file references, with secret-looking values redacted -- so a
    missing or empty required variable (like a :?-guarded host port) is
    obvious.
  - A VRAM-fit check against the node's free VRAM (reusing citadel #833's
    memory.free signal). Pass --need-mb to check a specific requirement;
    without it, this check reports "unknown".
  - Known-pattern hints (trust_remote_code, CUDA OOM, missing model files,
    port conflicts, permission errors) detected heuristically in the log.
  - A single most-likely cause + suggested next action.

This is read-only: it never starts, stops, or changes anything. Every check
degrades to "unknown" independently rather than failing the whole report.`,
	Example: `  citadel service diagnose vllm
  citadel service diagnose vllm --need-mb 16000
  citadel service diagnose vllm --json`,
	Args: cobra.ExactArgs(1),
	RunE: runServiceDiagnose,
}

func init() {
	svcCmd.AddCommand(serviceDiagnoseCmd)
	serviceDiagnoseCmd.Flags().BoolVar(&diagnoseJSON, "json", false, "Output the diagnosis as JSON")
	serviceDiagnoseCmd.Flags().IntVar(&diagnoseNeedMB, "need-mb", 0, "VRAM (MB) this service needs, to check fit against free VRAM (omit to skip the VRAM-fit verdict)")
	serviceDiagnoseCmd.Flags().IntVar(&diagnoseTail, "tail", 200, "Number of log lines to inspect")
}

func runServiceDiagnose(cmd *cobra.Command, args []string) error {
	name := args[0]
	// Reuse logs.go's validator: same argument-injection concern (this name
	// eventually reaches docker/journalctl exec args).
	if !validServiceNameRe.MatchString(name) {
		return fmt.Errorf("invalid service name %q: must be alphanumeric with hyphens, dots, or underscores", name)
	}

	in := gatherDiagnoseInput(name)
	report := diagnose.Diagnose(in)

	if diagnoseJSON {
		return renderDiagnoseJSON(cmd.OutOrStdout(), report)
	}
	renderDiagnoseReport(cmd.OutOrStdout(), report)
	return nil
}

// gatherDiagnoseInput collects everything diagnose.Diagnose needs from
// docker/compose/manifest/status. It is deliberately the ONLY place in this
// file that shells out or touches the filesystem -- diagnose.Diagnose itself
// stays pure. Every step degrades independently: a missing manifest, a
// docker failure, or no GPU still produces a best-effort Input rather than an
// error.
func gatherDiagnoseInput(name string) diagnose.Input {
	in := diagnose.Input{ServiceName: name}

	// Defense in depth: runServiceDiagnose already rejects an invalid name
	// before calling here, but this function is also the reusable seam #852
	// asks for (citadel doctor / an MCP tool could call it directly), so an
	// attacker-influenced name must never reach docker/compose exec argv
	// even if a future caller skips the CLI's own check.
	if !validServiceNameRe.MatchString(name) {
		return in
	}

	// 1. Resolve the compose path from the manifest, if this is a managed
	// (docker compose) service. Compose file paths in citadel.yaml are
	// relative to the manifest location (see cmd/logs.go's identical join).
	var composePath string
	if manifest, configDir, err := findAndReadManifest(); err == nil && manifest != nil {
		for _, s := range manifest.Services {
			if s.Name == name {
				in.Managed = true
				if s.ComposeFile != "" {
					composePath = filepath.Join(configDir, s.ComposeFile)
				}
				break
			}
		}
	}
	if composePath != "" {
		if raw, err := os.ReadFile(composePath); err == nil {
			in.ComposeRaw = string(raw)
		}
	}

	// 2. Resolve the container name. Prefer compose ps --all (handles the
	// #692 project-scoping gotcha + non-"citadel-<name>" containers like
	// whatsapp-bridge's "bridge"/"db" -- and, critically for a diagnose
	// command, an EXITED container: a bare `ps` without --all only lists
	// running containers, which is exactly the state diagnose exists to
	// look at). Falls back to the "citadel-<name>" convention when compose
	// ps can't be run (docker unreachable, service unmanaged, no compose
	// file, ...).
	containerName := "citadel-" + name
	if composePath != "" {
		if resolved, ok := resolveContainerName(composePath, name); ok {
			containerName = resolved
		}
	}

	// 3. Container state (exists/status/exit code/docker's own error).
	in.Container = inspectContainer(containerName)

	// 4. Log tail, bounded, only when the container exists AND `docker logs`
	// itself succeeds (a failed docker CLI invocation must never be mistaken
	// for the container's own log -- see containerLogTail).
	if in.Container.Exists {
		if tail, ok := containerLogTail(containerName, diagnoseTail); ok {
			in.LogTail = tail
		}
	}

	// 5. Resolved env: what the compose file's ${VAR...} tokens actually
	// resolve against on THIS node. Layered lowest-first: the install-time
	// sibling <name>.env (compose.EnvFileArgs' --env-file target), then the
	// process env citadel injects (composeEnv) on top -- citadel's own
	// injected host-port/workspace vars are what should win by construction
	// of this merge; this is not a claim about docker compose's internal
	// --env-file-vs-ambient-env precedence. This layer matters concretely:
	// SERVICE_START model persistence (#530) writes a swapped VLLM_MODEL to
	// <name>.env, so without it diagnose would print the compose file's
	// stale ${VLLM_MODEL:-...} default instead of the model actually being
	// served -- exactly the "embedding model served with a chat command"
	// root cause from the incident that motivated #852.
	//
	// EnvFileKeys separately records which of these keys came from the env
	// file, so diagnose.Diagnose can redact them unconditionally (that file
	// is documented as secret-bearing -- catalog installs write API keys
	// there -- with no naming convention citadel controls to pattern-match
	// on; see internal/diagnose.isSecretVar).
	resolvedEnv := map[string]string{}
	envFileKeys := map[string]bool{}
	if composePath != "" {
		for k, v := range envFileToMap(compose.SiblingEnvPath(composePath)) {
			resolvedEnv[k] = v
			envFileKeys[k] = true
		}
	}
	for k, v := range envToMap(composeEnv()) {
		resolvedEnv[k] = v
	}
	in.ResolvedEnv = resolvedEnv
	in.EnvFileKeys = envFileKeys

	// 6. Free VRAM, reusing citadel #833's signal via the same status
	// collector 'citadel services' and the #577 preemption path use.
	if st, err := status.NewCollector(status.CollectorConfig{}).Collect(); err == nil {
		if mb, ok := freeVRAMMB(st.GPU); ok {
			in.FreeVRAMMB = mb
			in.HaveFreeVRAM = true
		}
	}
	if diagnoseNeedMB > 0 {
		in.NeedVRAMMB = diagnoseNeedMB
		in.HaveNeedVRAM = true
	}

	return in
}

// resolveContainerName finds the container docker compose associates with
// serviceName in this compose file, INCLUDING one that has exited or is
// otherwise stopped. Deliberately does NOT reuse cmd/service.go's
// composeServiceState/compose.ResolveServiceState: that helper is shared with
// the live #692 status path and intentionally scopes to running containers
// only (plus the native-serving fallback), and changing its behavior for
// diagnose's sake would change status/services output too. --all is
// diagnose-specific, kept local to this file.
func resolveContainerName(composePath, serviceName string) (string, bool) {
	psArgs := append(composeFileArgs(composePath, composePath), "ps", "--all", "--format", "json")
	output, err := composeCommand(psArgs...).Output()
	if err != nil {
		return "", false
	}
	declared := compose.DeclaredServices(composePath)

	// --all can return MORE THAN ONE record for this service: nodes
	// accumulate stale/legacy containers (see removeLegacyCitadelProject's
	// doc comment), and PSContainer carries no timestamp to sort by. Prefer
	// a running/non-exited match -- the live container, if any, is what we
	// actually want to inspect -- and otherwise fall back to the LAST match
	// in whatever order compose emitted (closer to "most recently listed"
	// than the first, though that ordering is compose's, not a guarantee
	// this function makes).
	var lastMatch *compose.PSContainer
	for _, c := range compose.FilterPS(compose.ParsePS(output), declared) {
		if c.Service != serviceName {
			continue
		}
		if c.Running() {
			return c.Name, true
		}
		cc := c
		lastMatch = &cc
	}
	if lastMatch != nil {
		return lastMatch.Name, true
	}
	return "", false
}

// envFileToMap parses a docker "--env-file" style file (KEY=VALUE per line,
// blank lines and '#'-prefixed comments ignored) into a map. Best-effort: a
// missing/unreadable file yields an empty map, matching
// compose.EnvFileArgs' own "no sibling -> omit" behavior rather than erroring.
func envFileToMap(path string) map[string]string {
	m := map[string]string{}
	if path == "" {
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if k, v, ok := strings.Cut(trimmed, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// inspectContainer docker-inspects containerName for state/exit-code/error.
// A failed inspect (most commonly "No such container") is normal -- the
// service was never started, or its container was removed -- and reported as
// Exists=false, never a Go error: diagnose must never fail outright just
// because there's nothing to inspect.
func inspectContainer(containerName string) diagnose.ContainerState {
	rt := catalog.SelectContainerRuntime()
	out, err := exec.Command(rt.EngineBin, "inspect",
		"--format", "{{.State.Status}}|{{.State.ExitCode}}|{{.State.Error}}",
		containerName).Output()
	if err != nil {
		return diagnose.ContainerState{Exists: false, Name: containerName}
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	cs := diagnose.ContainerState{Exists: true, Name: containerName}
	if len(parts) > 0 {
		cs.Status = parts[0]
	}
	if len(parts) > 1 {
		if code, convErr := strconv.Atoi(parts[1]); convErr == nil {
			cs.ExitCode = code
		}
	}
	if len(parts) > 2 {
		cs.Error = parts[2]
	}
	return cs
}

// containerLogTail returns the last `tail` lines of the container's combined
// stdout/stderr, and false if `docker logs` itself failed (docker missing,
// container removed between inspect and here). The bool matters: on failure
// the CombinedOutput bytes are the docker CLI's OWN error message (e.g.
// "Error response from daemon: no such container"), not the container's log
// -- treating that as LogTail would make ExtractRootError report docker's
// error as the service's root cause. diagnose.Diagnose treats ok=false as "no
// log tail available" rather than failing.
func containerLogTail(containerName string, tail int) (string, bool) {
	if tail <= 0 {
		tail = 200
	}
	rt := catalog.SelectContainerRuntime()
	out, err := exec.Command(rt.EngineBin, "logs", "--tail", strconv.Itoa(tail), containerName).CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// envToMap turns a "KEY=VALUE" env slice (os.Environ()/composeEnv() shape)
// into a lookup map. A later duplicate KEY wins, matching how a process
// environment itself resolves duplicates.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// freeVRAMMB sums free VRAM (MB) across GPUs that report a memory total,
// preferring citadel #833's direct MemoryFreeMB signal over the derived
// total-minus-used value, for the same reason internal/jobs.freeVRAMBytes and
// cmd/hotswap.go's freeVRAMBytesFromGPU do: nvidia-smi reserves memory that
// counts against neither total nor used, so the derived value systematically
// overstates what's free -- the wrong direction of error for a check meant to
// catch an OOM/placement mistake before it happens. Returns ok=false when no
// GPU reports a memory total (no GPU / nvidia-smi absent).
//
// This is a third copy of the same handful of lines (see the two siblings
// above), not a new one: internal/resmon.Collect is a heavier probe that also
// walks processes/containers for per-owner VRAM attribution, which diagnose
// doesn't need just to answer "how much is free" -- pulling it in would cost
// more work per invocation for no extra signal here.
func freeVRAMMB(gpus []status.GPUMetrics) (int, bool) {
	total := 0
	found := false
	for _, g := range gpus {
		if g.MemoryTotalMB <= 0 {
			continue
		}
		found = true
		f := g.MemoryFreeMB
		if f <= 0 {
			f = g.MemoryTotalMB - g.MemoryUsedMB
		}
		if f < 0 {
			f = 0
		}
		total += f
	}
	return total, found
}

func renderDiagnoseJSON(w io.Writer, r diagnose.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func renderDiagnoseReport(w io.Writer, r diagnose.Report) {
	headerColor.Fprintf(w, "--- Diagnosing %s ---\n\n", r.ServiceName)

	headerColor.Fprintln(w, "CONTAINER")
	if !r.Container.Exists {
		fmt.Fprintf(w, "  %s no container found (looked for: %s)\n", warnColor.Sprint("[?]"), r.Container.Name)
	} else {
		statusBadge := goodColor.Sprint("[OK]")
		if !strings.EqualFold(r.Container.Status, "running") {
			statusBadge = badColor.Sprint("[FAIL]")
		}
		fmt.Fprintf(w, "  %s state=%s exit_code=%d name=%s\n", statusBadge, r.Container.Status, r.Container.ExitCode, r.Container.Name)
		if r.Container.Error != "" {
			fmt.Fprintf(w, "  docker error: %s\n", r.Container.Error)
		}
	}
	if r.RootError != "" {
		fmt.Fprintf(w, "  root error: %s\n", r.RootError)
	}

	if r.ComposeCommand != "" {
		headerColor.Fprintln(w, "\nCOMPOSE COMMAND")
		fmt.Fprintf(w, "  %s\n", r.ComposeCommand)
	}

	if len(r.EnvChecks) > 0 {
		headerColor.Fprintln(w, "\nENV / COMPOSE VARIABLES")
		for _, ec := range r.EnvChecks {
			statusBadge := goodColor.Sprint("[OK]")
			if ec.Verdict != diagnose.EnvOK {
				statusBadge = badColor.Sprint("[FAIL]")
			}
			val := ec.Value
			if val == "" {
				val = "<empty>"
			}
			req := ""
			if ec.Required {
				req = " (required)"
			}
			fmt.Fprintf(w, "  %s %s=%s%s [%s]\n", statusBadge, ec.Var, val, req, ec.Verdict)
		}
	}

	headerColor.Fprintln(w, "\nVRAM FIT")
	switch r.VRAM.Verdict {
	case diagnose.VRAMFits:
		fmt.Fprintf(w, "  %s %dMB free >= %dMB needed\n", goodColor.Sprint("[OK]"), r.VRAM.FreeMB, r.VRAM.NeedMB)
	case diagnose.VRAMInsufficient:
		fmt.Fprintf(w, "  %s %dMB free < %dMB needed\n", badColor.Sprint("[FAIL]"), r.VRAM.FreeMB, r.VRAM.NeedMB)
	default:
		if r.VRAM.HaveFree {
			fmt.Fprintf(w, "  %s unknown need (pass --need-mb to check fit; %dMB currently free)\n", warnColor.Sprint("[?]"), r.VRAM.FreeMB)
		} else {
			fmt.Fprintf(w, "  %s unknown (no GPU / free-VRAM signal available)\n", warnColor.Sprint("[?]"))
		}
	}

	if len(r.Hints) > 0 {
		headerColor.Fprintln(w, "\nHINTS")
		for _, h := range r.Hints {
			fmt.Fprintf(w, "  - %s\n", h)
		}
	}

	fmt.Fprintln(w)
	headerColor.Fprintln(w, "DIAGNOSIS")
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Most likely cause:"), r.MostLikelyCause)
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Suggested action:"), r.SuggestedAction)
}
