// cmd/service_diagnose.go
//
// `citadel service diagnose <name>` (citadel #852) is a one-command triage
// for a managed service that's down or unhealthy: container state + exit
// code, the salient root-error line from its log tail, the effective compose
// command/env (secrets redacted), best-effort preflight checks (missing
// required env, VRAM fit, cheap error-pattern hints), and a synthesized
// most-likely-cause + suggested next action.
//
// It is deliberately a subcommand of the EXISTING `citadel service` (alias
// `svc`) parent -- which otherwise manages Citadel itself as a system service
// (install/start/stop) -- rather than a new top-level command, per the issue.
// The two concepts are unrelated (this diagnoses a managed docker-compose AI
// engine like vllm/bonsai, not the citadel binary's own systemd/launchd
// unit); if that reads as a surprising place to look, `citadel services`
// (plural, cmd/services_cmd.go) is the sibling command for a live per-service
// usage/idle table.
//
// All logic that reasons about the gathered facts lives in
// internal/servicediag, which is pure/injectable and therefore independently
// unit-tested. This file's job is narrow: read the manifest, resolve the
// compose file + env, shell out to docker (read-only: inspect + logs only,
// NEVER up/down/restart/rm), and render. It never starts/stops/restarts
// anything, never mutates the manifest, and never writes a file.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/resmon"
	"github.com/aceteam-ai/citadel-cli/internal/servicediag"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
)

var (
	svcDiagnoseJSON bool
	svcDiagnoseTail int
)

var svcDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <name>",
	Short: "Diagnose why a managed service is down or unhealthy",
	Long: `Reports, in one command, why a MANAGED service (declared in citadel.yaml or
available from the embedded catalog) is down or behaving unexpectedly:

  - Container state + exit code, and the salient root-error line pulled from
    the tail of its log.
  - The effective compose command + resolved environment (secrets redacted).
  - Best-effort preflight checks: a missing/empty required compose variable,
    whether the service's declared VRAM need fits current free VRAM, and
    cheap error-pattern hints (OOM, trust_remote_code, port-in-use, ...).
  - A single synthesized most-likely cause and suggested next action.

This is read-only: it never starts, stops, restarts, or reconfigures
anything. Every check degrades to "unknown" rather than failing the command
when its input (docker, GPU, a log) isn't available.`,
	Args: cobra.ExactArgs(1),
	RunE: runSvcDiagnose,
}

func init() {
	svcCmd.AddCommand(svcDiagnoseCmd)
	svcDiagnoseCmd.Flags().BoolVar(&svcDiagnoseJSON, "json", false, "Output the diagnosis as JSON")
	svcDiagnoseCmd.Flags().IntVar(&svcDiagnoseTail, "tail", 200, "Number of log lines to fetch/scan from the container's log tail")
}

func runSvcDiagnose(_ *cobra.Command, args []string) error {
	name := args[0]
	if !validServiceNameRe.MatchString(name) {
		return fmt.Errorf("invalid service name %q: must be alphanumeric with hyphens, dots, or underscores", name)
	}

	manifest, configDir, _ := findAndReadManifest() // best-effort; nil manifest is fine
	var manifestNames []string
	if manifest != nil {
		for _, s := range manifest.Services {
			manifestNames = append(manifestNames, s.Name)
		}
	}

	managed, source := servicediag.IsManaged(name, manifestNames)
	if !managed {
		return fmt.Errorf("%q is not a managed service.\n\n%s", name, unmanagedServiceGuidance(manifestNames))
	}

	in, err := buildDiagnoseInput(name, manifest, configDir, manifestNames)
	if err != nil {
		return err
	}
	insp := resolveInspector()
	report := servicediag.Diagnose(in, insp)
	_ = source // already reflected in report.ManagedSource

	if svcDiagnoseJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderDiagnoseReport(os.Stdout, report)
	return nil
}

// unmanagedServiceGuidance lists what IS managed, so an operator/agent that
// mistyped a name (or asked about an ad-hoc container diagnose deliberately
// does not reason about) has an immediate next step.
func unmanagedServiceGuidance(manifestNames []string) string {
	set := map[string]bool{}
	var all []string
	for _, n := range manifestNames {
		if !set[n] {
			set[n] = true
			all = append(all, n)
		}
	}
	for _, n := range services.GetAvailableServices() {
		if !set[n] {
			set[n] = true
			all = append(all, n)
		}
	}
	sort.Strings(all)
	return fmt.Sprintf("Managed services: %s\nSee 'citadel services' for what's actually running on this node.",
		strings.Join(all, ", "))
}

// buildDiagnoseInput gathers every fact servicediag.Diagnose needs: the
// compose content (from disk if materialized, else the embedded catalog),
// the resolved environment (sibling .env file, overridden by process env --
// mirrors docker compose's own precedence), and the VRAM signals (citadel
// #833's free-VRAM reporting via resmon, and the coarse per-engine
// provisioning budget table).
//
// Returns an error (never a zero-value Input alongside a nil error) when
// diagnosing under an active --node-dir/CITADEL_NODE_DIR override would
// silently target the REAL node's global container -- see
// diagnoseNodeDirRefusalError.
func buildDiagnoseInput(name string, manifest *CitadelManifest, configDir string, manifestNames []string) (servicediag.Input, error) {
	in := servicediag.Input{
		ServiceName:          name,
		ContainerName:        "citadel-" + name,
		ManifestServiceNames: manifestNames,
		MaxLogLines:          svcDiagnoseTail,
	}

	composePath, content, source := resolveComposeContent(name, manifest, configDir)
	in.ComposeFilePath = composePath
	in.ComposeContent = content
	in.ComposeSource = source

	// An external/module compose file is free to declare its own
	// `container_name:` (catalog.ParseComposeContainerName is the same
	// extraction `citadel module install` uses to check for name conflicts),
	// which overrides this repo's "citadel-<service>" convention. Diagnosing
	// against the wrong container name reports a false "no container found"
	// for a module-installed service whose compose renamed it.
	//
	// Only trust the override when the compose declares exactly ONE
	// container_name: -- ParseComposeContainerName returns the FIRST match
	// with no notion of which `services:` key it belongs to. A multi-service
	// module compose (e.g. services/nvr-service/compose.yml: wyze-bridge,
	// nvr-config, mosquitto, frigate each with their own container_name) has
	// no reliable way here to know which one corresponds to in.ServiceName,
	// and silently diagnosing an unrelated sibling container would be worse
	// than the old, honestly-wrong "citadel-<service>" guess.
	//
	// KNOWN LIMITATION: this guard is a text-level count of container_name:
	// occurrences, not a parsed count of `services:` entries -- it does not
	// catch a multi-service compose where only ONE service happens to declare
	// a container_name: at all. In that (currently unseen in this repo's own
	// compose files) shape, the count is 1 but it may not belong to
	// in.ServiceName, and the override would still apply. Resolving that
	// properly needs the same structured per-service compose decode
	// buildComposeInfo already does (doc.Services[in.ServiceName]) rather
	// than a package-boundary text scan; not worth pulling that parser across
	// the cmd/internal/catalog boundary for a shape that doesn't exist yet.
	if len(content) > 0 && strings.Count(string(content), "container_name:") == 1 {
		if cn := catalog.ParseComposeContainerName(string(content)); cn != "" {
			in.ContainerName = cn
		}
	}

	// citadel#863 (follow-up review of #862/#860): refuse rather than
	// silently diagnosing the REAL node's container. See
	// diagnoseNodeDirRefusalError for the exact predicate and rationale; a
	// no-op when no --node-dir/CITADEL_NODE_DIR override is active.
	if err := diagnoseNodeDirRefusalError(name, resolveNodeDirOverride(), source, in.ContainerName); err != nil {
		return servicediag.Input{}, err
	}

	in.ResolvedEnv = resolveEnvForCompose(composePath)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	snap := resmon.CollectWithManaged(ctx, manifestNames)
	in.FreeVRAMBytes = snap.GPU.FreeBytes
	in.FreeVRAMKnown = snap.HasGPU

	if engine := status.EngineTypeFromName(name); engine != "" {
		if mb := status.EngineVRAMEstimateMB(engine); mb > 0 {
			in.DeclaredVRAMNeedMB = mb
			in.DeclaredVRAMNeedKnown = true
		}
	}

	return in, nil
}

// diagnoseNodeDirRefusalError returns a non-nil error when diagnosing name
// under an active --node-dir/CITADEL_NODE_DIR override (override != "")
// would silently target the REAL node's global "citadel-<name>" container
// instead of anything isolated to the override.
//
// source is resolveComposeContent's third return value. "manifest" is the
// ONLY source that means "read from an actual file inside the override's
// resolved configDir" -- findAndReadManifest/findOrCreateManifest already
// make configDir --node-dir-aware (cmd/nodedir.go), so a "manifest" source
// is a real, override-owned, on-disk compose file, and (per citadel#860)
// citadel's OWN materialization writes it with a namespaced container_name
// the moment `citadel run`/`module start` under this override touches it.
// Anything else -- "embedded" (services.ServiceMap fallback: the common
// first-diagnose-before-first-start case the issue describes) or ""
// (nothing found at all) -- means the override directory has never
// materialized a compose file for this service, so in.ContainerName is
// still the bare, unnamespaced "citadel-<name>" convention (or a name
// parsed from the UNmaterialized embedded template, which is the same
// unnamespaced convention) -- exactly the name a REAL node's container on
// this same Docker daemon carries. servicediag.Diagnose always inspects and
// tails logs from in.ContainerName unconditionally (it does not gate on
// whether compose content was found), so without this refusal the command
// would read-only-but-silently disclose the real container's state and log
// tail to an operator who believes they're diagnosing an isolated override
// service -- the exact hazard citadel#863 tracks. Mirrors
// stopServiceByContainer's "refuse under active override" pattern
// (cmd/stop.go, citadel#856 review) for the identical reason: a bare
// container-name-keyed docker operation has no compose-project scope
// (composeArgsWithProject/#856) to protect it.
//
// No-op (nil) whenever override == "" -- the no-override path is
// byte-identical to pre-#863 behavior, matching every other --node-dir
// guard in this codebase.
func diagnoseNodeDirRefusalError(name, override, source, containerName string) error {
	if override == "" || source == "manifest" {
		return nil
	}
	return fmt.Errorf(
		"refusing to diagnose %q under --node-dir/CITADEL_NODE_DIR %q: no compose file for it has been "+
			"materialized inside the override directory yet, so there is nothing isolated here to diagnose -- "+
			"diagnosing now would inspect and tail logs from %q, which (absent materialization under this "+
			"override) is the node's REAL, unnamespaced container. Start the service under this override first "+
			"(e.g. 'citadel run %s --node-dir %s'), then re-run diagnose, or drop --node-dir to diagnose the "+
			"default node.",
		name, override, containerName, name, override)
}

// resolveComposeContent finds the compose definition for name WITHOUT ever
// writing one: it reads an already-materialized manifest-declared file from
// disk if present, and otherwise falls back to the in-binary embedded
// catalog entry (services.ServiceMap) -- unlike ensureComposeFile, it never
// extracts/writes that content to the node's services directory. That is
// what keeps `diagnose` side-effect-free for a catalog service that has never
// been started.
func resolveComposeContent(name string, manifest *CitadelManifest, configDir string) (path string, content []byte, source string) {
	if manifest != nil {
		for _, s := range manifest.Services {
			if s.Name != name || s.ComposeFile == "" {
				continue
			}
			full := filepath.Join(configDir, s.ComposeFile)
			if data, err := os.ReadFile(full); err == nil {
				return full, data, "manifest"
			}
			break
		}
	}
	if raw, ok := services.ServiceMap[name]; ok {
		return "", []byte(raw), "embedded"
	}
	return "", nil, ""
}

// resolveEnvForCompose merges the service's sibling <name>.env file (lowest
// precedence, if it exists) with the process environment composeEnv() would
// supply to a real `docker compose` invocation (highest precedence) -- the
// same order docker compose itself resolves interpolation in: shell/process
// env overrides an --env-file. composePath may be "" (no on-disk file);
// resolveEnvForCompose degrades to just the process env in that case.
func resolveEnvForCompose(composePath string) map[string]string {
	env := map[string]string{}
	if composePath != "" {
		envPath := compose.SiblingEnvPath(composePath)
		if data, err := os.ReadFile(envPath); err == nil {
			for k, v := range servicediag.ParseDotEnv(data) {
				env[k] = v
			}
		}
	}
	for _, kv := range composeEnv() {
		if k, v, found := strings.Cut(kv, "="); found {
			env[k] = v
		}
	}
	return env
}

// resolveInspector returns a real docker/podman-backed Inspector, or nil when
// the engine is unusable -- servicediag.Diagnose degrades every dependent
// field to "unknown" for a nil Inspector rather than failing.
func resolveInspector() servicediag.Inspector {
	rt := catalog.SelectContainerRuntime()
	if health := platform.CheckDockerUsable(rt.EngineBin); !health.OK {
		return nil
	}
	return servicediag.NewDockerInspector(rt.EngineBin)
}

// renderDiagnoseReport prints a human-readable diagnosis to w.
func renderDiagnoseReport(w *os.File, r servicediag.Report) {
	headerColor.Fprintf(w, "--- 🔎 Diagnosing %s ---\n", r.Service)
	fmt.Fprintf(w, "%s %s", labelColor.Sprint("Managed:"), yesNo(r.Managed))
	if r.ManagedSource != "" {
		fmt.Fprintf(w, " (%s)", r.ManagedSource)
	}
	fmt.Fprintln(w)

	headerColor.Fprintln(w, "\nCONTAINER")
	printContainerState(w, r.Container)

	headerColor.Fprintln(w, "\nLOG TAIL")
	printLogTail(w, r.Logs, r.Container.Running)

	headerColor.Fprintln(w, "\nCOMPOSE")
	printComposeInfo(w, r.Compose)

	headerColor.Fprintln(w, "\nPREFLIGHT CHECKS")
	printChecks(w, r.Checks)

	if len(r.Hints) > 0 {
		headerColor.Fprintln(w, "\nHINTS")
		for _, h := range r.Hints {
			fmt.Fprintf(w, "  - %s\n", h)
		}
	}

	fmt.Fprintln(w)
	labelColor.Fprint(w, "Most likely cause: ")
	fmt.Fprintln(w, r.Verdict)
	labelColor.Fprint(w, "Suggested next step: ")
	fmt.Fprintln(w, r.NextAction)
}

func printContainerState(w *os.File, cs servicediag.ContainerState) {
	if cs.Error != "" {
		fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[UNKNOWN]"), cs.Error)
		return
	}
	if !cs.Found {
		fmt.Fprintf(w, "  %s no container named this service was found\n", warnColor.Sprint("[NOT FOUND]"))
		return
	}
	badge := goodColor.Sprint("[RUNNING]")
	if !cs.Running {
		badge = badColor.Sprint("[STOPPED]")
	}
	fmt.Fprintf(w, "  %s status=%s exit_code=%d\n", badge, cs.Status, cs.ExitCode)
}

// printLogTail renders the log tail. running is r.Container.Running: a
// matched error-looking line in an otherwise-healthy running container is
// informational, not a claimed cause, so it gets a neutral label instead of
// the "root error:" phrasing reserved for a non-running container -- keeping
// this in sync with synthesize's identical running/not-running distinction.
func printLogTail(w *os.File, l servicediag.LogTail, running bool) {
	if l.Error != "" {
		fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[UNKNOWN]"), l.Error)
		return
	}
	if !l.Available {
		fmt.Fprintln(w, "  (no log lines available)")
		return
	}
	if l.RootError != "" {
		if running {
			fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("error-looking line (container is running):"), l.RootError)
		} else {
			fmt.Fprintf(w, "  %s %s\n", badColor.Sprint("root error:"), l.RootError)
		}
	} else {
		fmt.Fprintln(w, faintColor.Sprint("  (no specific error line matched a known pattern; see full tail below)"))
	}
	fmt.Fprintf(w, "  --- last %d line(s) ---\n", len(l.Lines))
	for _, line := range l.Lines {
		fmt.Fprintf(w, "  %s\n", faintColor.Sprint(line))
	}
}

func printComposeInfo(w *os.File, c servicediag.ComposeInfo) {
	if c.ParseError != "" {
		fmt.Fprintf(w, "  %s could not parse compose file: %s\n", warnColor.Sprint("[UNKNOWN]"), c.ParseError)
		return
	}
	if c.Source == "" {
		fmt.Fprintln(w, "  (no compose definition available)")
		return
	}
	fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Source:"), c.Source)
	if c.ComposeFilePath != "" {
		fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("File:"), c.ComposeFilePath)
	}
	if c.Command != "" {
		fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Command:"), c.Command)
	}
	if len(c.Env) > 0 {
		fmt.Fprintln(w, "  Environment:")
		keys := make([]string, 0, len(c.Env))
		for k := range c.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tw := tabwriter.NewWriter(w, 4, 2, 2, ' ', 0)
		for _, k := range keys {
			fmt.Fprintf(tw, "    %s=\t%s\n", k, c.Env[k])
		}
		tw.Flush()
	}
}

func printChecks(w *os.File, checks []servicediag.PreflightCheck) {
	if len(checks) == 0 {
		fmt.Fprintln(w, "  (no checks applicable)")
		return
	}
	for _, c := range checks {
		fmt.Fprintf(w, "  %s %s: %s\n", verdictBadge(c.Verdict), c.Name, c.Detail)
	}
}

func verdictBadge(v string) string {
	switch v {
	case servicediag.VerdictOK:
		return goodColor.Sprint("[OK]")
	case servicediag.VerdictFail:
		return badColor.Sprint("[FAIL]")
	case servicediag.VerdictWarn:
		return warnColor.Sprint("[WARN]")
	default:
		return faintColor.Sprint("[UNKNOWN]")
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
