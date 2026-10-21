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
		{"worker_validation", "internal/worker/run_job_template.go", "runner, err := ValidateTemplateRunRequest(req, h.cfg.NodeID)\n\tif err != nil", "runner, err := ValidateTemplateRunRequest(req, h.cfg.NodeID)\n\tif false && err != nil", "./internal/worker", "TestRunJobTemplate_InputContractRefusedBeforeOps/wrong_property_type"},
		{"approved_hash", "internal/worker/run_job_template.go", "if !hashesEqual(recomputed, req.ContentHash)", "if false && !hashesEqual(recomputed, req.ContentHash)", "./internal/worker", "TestRunJobTemplate_RefusalPathsNeverRunOps/hash_mismatch_refused"},
		{"builtin_only", "internal/worker/run_job_template.go", "if runner.Kind != \"builtin\"", "if false && runner.Kind != \"builtin\"", "./internal/worker", "TestRunJobTemplate_RefusalPathsNeverRunOps/shell_kind_refused"},
		{"exact_runner_keys", "internal/jobs/template_contract.go", "if k != \"kind\" && k != \"handler\"", "if false && k != \"kind\" && k != \"handler\"", "./internal/worker", "TestReviewRunnerReorderingCannotChangeHandler"},
		{"node_identity", "internal/jobs/template_contract.go", "f.NodeID != nodeID", "false", "./cmd", "TestReviewForeignNodeInputRefused"},
		{"params_schema", "internal/jobs/template_contract.go", "if err := schema.Validate(v); err != nil", "if err := schema.Validate(v); false && err != nil", "./internal/worker", "TestRunJobTemplate_InputContractRefusedBeforeOps/wrong_property_type"},
		{"adapter_validation", "cmd/run_job_template_ops.go", "worker.ValidateTemplateRunRequest(req, o.nodeID)", "jobs.ParseTemplateRunner(req.Runner)", "./cmd", "TestLiveTemplateRunOps_RefusesBeforeEffects/invalid_schema_params"},
		{"python_numbers", "internal/jobs/template_hash.go", "buf.WriteString(pythonFloat(f))", "buf.WriteString(string(val))", "./internal/jobs", "TestReviewPythonNumberParity"},
		{"output_limits", "cmd/run_job_template_ops.go", "if err := validateTemplateOutputTree(outRoot, outRels, maxFiles, maxEntries, maxBytes); err != nil", "if err := error(nil); err != nil", "./cmd", "TestLiveTemplateRunOps_OutputCountFailureCleansAttempt"},
		{"hashed_schema_numbers", "internal/jobs/template_contract.go", "bytes.NewReader(canonical.Bytes())", "bytes.NewReader(field.raw)", "./internal/jobs", "TestTemplateSchemaUsesHashedNumericSemantics"},
		{"canonical_builtin_params", "cmd/run_job_template_ops.go", "req.Params, err = jobs.NormalizeTemplateParams(req.Params)", "req.Params, err = req.Params, nil", "./cmd", "TestLiveTemplateRunOps_DispatchesCanonicalParams"},
		{"resolved_workspace_root", "cmd/run_job_template_ops.go", "workspaceDir, err = filepath.EvalSymlinks(workspaceDir)", "workspaceDir, err = workspaceDir, nil", "./cmd", "TestLiveTemplateRunOps_SymlinkWorkspaceRetargetCannotChangeStagedInput"},
		{"unique_attempt_namespace", "cmd/run_job_template_ops.go", "attempt, err := newTemplateAttemptSegment()", "attempt, err := \"attempt-fixed\", error(nil)", "./cmd", "TestLiveTemplateRunOps_CancelledAttemptCannotOverlapRetryNamespace"},
		{"cancellable_template_copy", "cmd/run_job_template_ops.go", "if nr > 0 {\n\t\t\tif err := ctx.Err(); err != nil {", "if nr > 0 {\n\t\t\tif err := ctx.Err(); false && err != nil {", "./cmd", "TestCopyTemplateWithContextStopsBetweenChunks"},
		{"film_closed_schema", "cmd/template_builtin_video_compose.go", "if err := schema.Validate(v); err != nil", "if err := schema.Validate(v); false && err != nil", "./cmd", "TestVideoComposeFilmClosedSchema/unknown_top"},
		{"film_disclosure_guard", "cmd/template_builtin_video_compose.go", "ch.SpeaksAsPerson && ch.Disclosure == nil", "false && ch.SpeaksAsPerson && ch.Disclosure == nil", "./cmd", "TestVideoComposeDisclosureGuard"},
		{"film_mixed_aspect", "cmd/template_builtin_video_compose.go", "i > 0 && !videoComposeSameAspect(media[0], media[i])", "false && i > 0 && !videoComposeSameAspect(media[0], media[i])", "./cmd", "TestVideoComposeMixedAspectRefused"},
		{"film_delivery_cap", "cmd/template_builtin_video_compose.go", "if info.Size() > film.Delivery.MaxBytes {", "if false && info.Size() > film.Delivery.MaxBytes {", "./cmd", "TestVideoComposeFakeMediaPipeline"},
		{"film_stream_copy", "cmd/template_builtin_video_compose.go", "a == b && a.VideoCodec", "true || a == b && a.VideoCodec", "./cmd", "TestVideoComposeStreamDecision"},
		{"film_card_silence", "cmd/template_builtin_papercraft.go", "} else if cfg.SilentAudio {", "} else if false && cfg.SilentAudio {", "./cmd", "TestVideoComposeSingleLandscapeCapture"},
		{"film_inactive_gate", "cmd/template_builtin_video_compose.go", "if deps.visuals == nil {", "if false && deps.visuals == nil {", "./cmd", "TestVideoComposeInactiveBeforeEffects"},
		{"film_declared_input", "cmd/template_builtin_video_compose.go", "chapters = append(chapters, files[ch.File])", "chapters = append(chapters, ch.File)", "./cmd", "TestVideoComposeBindsDeclaredFinalBasenames"},
		{"film_mp4_preflight", "cmd/template_builtin_video_compose.go", "if err := validateVideoComposeMP4(files[ch.File]); err != nil", "if err := validateVideoComposeMP4(files[ch.File]); false && err != nil", "./cmd", "TestVideoComposeRejectsDisguisedDASHBeforeMedia"},
		{"film_mov_demuxer", "cmd/template_builtin_video_compose.go", "\"-f\", \"mov\", \"-enable_drefs\"", "\"-f\", \"dash\", \"-enable_drefs\"", "./cmd", "TestVideoComposeMP4InputArgsPinned"},
		{"film_external_refs_disabled", "cmd/template_builtin_video_compose.go", "\"-enable_drefs\", \"0\"", "\"-enable_drefs\", \"1\"", "./cmd", "TestVideoComposeMP4InputArgsPinned"},
		{"film_mp4_self_contained", "cmd/template_builtin_video_compose.go", "binary.BigEndian.Uint32(data[16:20]) != 1", "false && binary.BigEndian.Uint32(data[16:20]) != 1", "./cmd", "TestVideoComposeMP4RejectsExternalReferences"},
		{"film_probe_container", "cmd/template_builtin_video_compose.go", "if probe.Format.Name != \"mov,mp4,m4a,3gp,3g2,mj2\"", "if false && probe.Format.Name != \"mov,mp4,m4a,3gp,3g2,mj2\"", "./cmd", "TestVideoComposeProbeRejectsWrongContainer"},
		{"film_poster_decoder", "cmd/template_builtin_video_compose.go", "\"-c:v\", \"png\", \"-i\", poster", "\"-c:v\", \"mjpeg\", \"-i\", poster", "./cmd", "TestVideoComposeReencodeAndPosterOverride"},
		{"film_required_expectations", "internal/worker/run_job_template.go", "if req.TemplateKey == \"video-compose\" || runner.Handler == \"video-compose\"", "if false && (req.TemplateKey == \"video-compose\" || runner.Handler == \"video-compose\")", "./internal/worker", "TestRunJobTemplate_FilmRequiresEveryInputExpectationBeforeOps"},
		{"film_expected_digest", "internal/jobs/template_contract.go", "if f.ExpectedSHA256 != \"\" && f.ExpectedSHA256 != digest", "if false && f.ExpectedSHA256 != \"\" && f.ExpectedSHA256 != digest", "./cmd", "TestLiveTemplateRunOps_FilmInputBindingRefusesBeforeBuiltin"},
		{"film_expected_size", "internal/jobs/template_contract.go", "if f.ExpectedBytes != nil && *f.ExpectedBytes != size", "if false && f.ExpectedBytes != nil && *f.ExpectedBytes != size", "./cmd", "TestLiveTemplateRunOps_FilmInputBindingRefusesBeforeBuiltin"},
		{"film_duplicate_expectations", "cmd/run_job_template_ops.go", "if err := f.VerifyExpectedContent(digest, size); err != nil", "if err := f.VerifyExpectedContent(digest, size); false && err != nil", "./cmd", "TestLiveTemplateRunOps_FilmInputBindingRefusesBeforeBuiltin"},
	}
	for _, m := range mutations {
		source := filepath.Join(root, m.file)
		raw, err := os.ReadFile(source)
		must(err)
		original := string(raw)
		index := strings.Index(original, m.from)
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
