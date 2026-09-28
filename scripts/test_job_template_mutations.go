//go:build ignore

// Run with: go run ./scripts/test_job_template_mutations.go
// Overlays leave the checkout unchanged. A mutation is killed only by an
// assertion failure in the named regression test, never by a compile error.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	root, err := os.Getwd()
	must(err)
	dir, err := os.MkdirTemp("", "citadel-template-mutations-")
	must(err)
	defer os.RemoveAll(dir)
	mutations := []struct{ name, file, from, to, pkg, test string }{
		{"worker_validation", "internal/worker/run_job_template.go", "if err != nil {\n\t\treturn h.failure(fmt.Errorf(\"RUN_JOB_TEMPLATE: %w\", err)), nil\n\t}", "if false && err != nil {\n\t\treturn h.failure(fmt.Errorf(\"RUN_JOB_TEMPLATE: %w\", err)), nil\n\t}", "./internal/worker", "TestReviewMalformedParamsRefusedBeforeOps"},
		{"approved_hash", "internal/worker/run_job_template.go", "if !hashesEqual(recomputed, req.ContentHash)", "if false && !hashesEqual(recomputed, req.ContentHash)", "./internal/worker", "TestRunJobTemplate_RefusalPathsNeverRunOps/hash_mismatch_refused"},
		{"builtin_only", "internal/worker/run_job_template.go", "if runner.Kind != \"builtin\"", "if false && runner.Kind != \"builtin\"", "./internal/worker", "TestRunJobTemplate_RefusalPathsNeverRunOps/shell_kind_refused"},
		{"exact_runner_keys", "internal/jobs/template_contract.go", "if k != \"kind\" && k != \"handler\"", "if false && k != \"kind\" && k != \"handler\"", "./internal/worker", "TestReviewRunnerReorderingCannotChangeHandler"},
		{"node_identity", "internal/jobs/template_contract.go", "f.NodeID != nodeID", "false", "./cmd", "TestReviewForeignNodeInputRefused"},
		{"params_schema", "internal/jobs/template_contract.go", "if err := schema.Validate(v); err != nil", "if err := schema.Validate(v); false && err != nil", "./internal/worker", "TestRunJobTemplate_InputContractRefusedBeforeOps/wrong_property_type"},
		{"adapter_validation", "cmd/run_job_template_ops.go", "worker.ValidateTemplateRunRequest(req, o.nodeID)", "jobs.ParseTemplateRunner(req.Runner)", "./cmd", "TestLiveTemplateRunOps_RefusesBeforeEffects/malformed_params"},
		{"python_numbers", "internal/jobs/template_hash.go", "buf.WriteString(pythonFloat(f))", "buf.WriteString(string(val))", "./internal/jobs", "TestReviewPythonNumberParity"},
		{"workspace_outputs", "cmd/run_job_template_ops.go", "jobs.ValidatePath(o.workspaceDir, filepath.Join(outDir, rel))", "filepath.Abs(filepath.Join(outDir, rel))", "./cmd", "TestLiveTemplateRunOps_OutputEscapingWorkspaceRejected"},
	}
	for _, m := range mutations {
		source := filepath.Join(root, m.file)
		raw, err := os.ReadFile(source)
		must(err)
		// The worker has two identical refusal blocks; target the final one after
		// ValidateTemplateRunRequest, not the earlier payload parsing block.
		original := string(raw)
		index := strings.Index(original, m.from)
		if m.name == "worker_validation" {
			index = strings.LastIndex(original, m.from)
		}
		if index < 0 {
			panic("mutation target missing: " + m.name)
		}
		mutant := original[:index] + m.to + original[index+len(m.from):]
		target := filepath.Join(dir, m.name+".go")
		must(os.WriteFile(target, []byte(mutant), 0600))
		overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: target}})
		must(err)
		overlayPath := filepath.Join(dir, "overlay.json")
		must(os.WriteFile(overlayPath, overlay, 0600))
		cmd := exec.Command("go", "test", "-overlay", overlayPath, m.pkg, "-run", "^"+m.test+"$", "-count=1")
		out, runErr := cmd.CombinedOutput()
		if runErr == nil || !strings.Contains(string(out), "--- FAIL: "+strings.Split(m.test, "/")[0]) {
			fmt.Fprintf(os.Stderr, "mutation %s survived or failed without the required assertion:\n%s", m.name, out)
			os.Exit(1)
		}
		fmt.Printf("KILLED %s (%s)\n", m.name, m.test)
	}
	fmt.Printf("%d/%d mutations killed by regression assertions\n", len(mutations), len(mutations))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
