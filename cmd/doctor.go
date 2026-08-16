// cmd/doctor.go
package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/spf13/cobra"
)

// doctorCmd is citadel-cli#767: detect a missing/unlinked container engine CLI
// (or an unreachable daemon) and suggest a platform-appropriate fix, instead of
// letting the first symptom be a raw exec error the next time a docker-based
// service tries to start (e.g. ollama on a node without a native install).
//
// It deliberately only PRINTS remediation commands; it never runs them. There
// is no established consent pattern in this codebase for auto-running
// system-modifying commands outside `citadel init`'s interactive provisioning
// prompts, and inventing one is out of scope here.
var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Aliases: []string{"dr"},
	Short:   "Diagnose common node problems and suggest fixes",
	Long: `Checks this node for known-problematic conditions and suggests remediation.

Currently checks the container engine (docker/podman) that docker-based
services -- most commonly ollama, when no native install is available -- need
to start.`,
	Example: `  citadel doctor`,
	Run: func(cmd *cobra.Command, args []string) {
		runDoctor()
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// runDoctor runs every doctor check and exits non-zero if any reported a
// problem, mirroring `citadel test`'s pass/fail exit convention so this is
// scriptable (e.g. a pre-flight check in an install script).
func runDoctor() {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	headerColor.Fprintf(w, "--- 🩺 Citadel Doctor ---\n")

	healthy := true
	headerColor.Fprintln(w, "\n🐳 CONTAINER ENGINE")
	if !doctorCheckContainerEngine(w) {
		healthy = false
	}

	w.Flush()
	if healthy {
		fmt.Println("\n✅ No problems found.")
	} else {
		fmt.Println("\n⚠️  citadel doctor found one or more problems — see remediation above.")
		os.Exit(1)
	}
}

// doctorCheckContainerEngine diagnoses the container engine citadel would
// actually exec for a docker-based service start (catalog.SelectContainerRuntime,
// the same resolution `citadel run`/the control center/SERVICE_START jobs use),
// and reports which configured services depend on it. Returns false when a
// problem was found.
func doctorCheckContainerEngine(w *tabwriter.Writer) bool {
	rt := catalog.SelectContainerRuntime()
	diag := platform.DiagnoseEngine(rt.EngineBin)

	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Runtime"), rt.Label())

	dependents := dockerDependentServiceNames()
	if len(dependents) > 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Depends on it"), joinOrNone(dependents))
	} else {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Depends on it"), "(no docker-based services configured yet)")
	}

	if diag.Healthy() {
		fmt.Fprintf(w, "  %s:\t%s (%s)\n", labelColor.Sprint("Status"), goodColor.Sprint("OK"), diag.CLIPath)
		return true
	}

	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), badColor.Sprint("PROBLEM"))
	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Issue"), warnColor.Sprint(diag.Diagnose("docker-based services cannot start")))
	fmt.Fprintf(w, "  %s:\n", labelColor.Sprint("Suggested fix"))
	for _, line := range diag.Remediate(platform.OS()) {
		fmt.Fprintf(w, "    %s\n", line)
	}
	return false
}

// dockerDependentServiceNames returns the names of manifest services that
// resolve to the docker-based kind (mirrors determineServiceType's
// auto-detect: explicit type wins, otherwise native-if-available). Returns nil
// (not an error) when there is no manifest yet -- a fresh node with nothing
// configured is not a doctor problem by itself.
func dockerDependentServiceNames() []string {
	manifest, _, err := findAndReadManifest()
	if err != nil || manifest == nil {
		return nil
	}
	var names []string
	for _, svc := range manifest.Services {
		if determineServiceType(svc) == services.ServiceTypeDocker {
			names = append(names, svc.Name)
		}
	}
	return names
}

// joinOrNone renders names as a comma-separated list, or "(none)" when empty.
func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}
