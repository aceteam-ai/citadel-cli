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

	in := buildDiagnoseInput(name, manifest, configDir, manifestNames)
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
func buildDiagnoseInput(name string, manifest *CitadelManifest, configDir string, manifestNames []string) servicediag.Input {
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

	return in
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
	printLogTail(w, r.Logs)

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

func printLogTail(w *os.File, l servicediag.LogTail) {
	if l.Error != "" {
		fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[UNKNOWN]"), l.Error)
		return
	}
	if !l.Available {
		fmt.Fprintln(w, "  (no log lines available)")
		return
	}
	if l.RootError != "" {
		fmt.Fprintf(w, "  %s %s\n", badColor.Sprint("root error:"), l.RootError)
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
