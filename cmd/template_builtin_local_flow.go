package cmd

// local-flow-run: run one pinned Flow package in the node's isolated Python
// local_flow engine container. The engine is a one-request stdin/stdout CLI: it
// reads {graph, package_digest, input, run_id}, writes {output, usage} or
// {error} on stdout, and reads CITADEL_LOCAL_GATEWAY_PORT, CITADEL_LOCAL_MODELS
// and CITADEL_LOCAL_RUNS_DIR from its environment. docs/local-flow-run.md owns
// the contract and its open activation gates.
//
// Every refusal happens before the container starts: the pinned template, the
// exact params, the node and organization binding, the package digest, the
// node-type allowlist, the served models, the pinned engine image, cgroup
// limit delegation and the run-id ledger. There is no fallback: a refused run
// never executes on hosted infrastructure or another node.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"crypto/rand"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

const (
	localFlowBuiltinName     = "local-flow-run"
	localFlowTemplateKey     = "local-flow-run"
	localFlowTemplateVersion = 1
	localFlowResultFile      = "result.json"

	// Bounds mirror aceteam#13130's admission contract (MAX_GRAPH_BYTES,
	// MAX_INPUT_BYTES, MAX_REQUEST_BYTES, MAX_OUTPUT_BYTES, MAX_INNER_NODES) so
	// the node refuses what the engine would refuse, before starting it.
	localFlowMaxGraphBytes   = 256 * 1024
	localFlowMaxInputBytes   = 1024 * 1024
	localFlowMaxRequestBytes = 1400 * 1024
	localFlowMaxOutputBytes  = 1024 * 1024
	localFlowMaxInnerNodes   = 32
	localFlowMaxEdges        = 128
	localFlowMaxModels       = 32
	// The engine writes one JSON line; anything beyond this is an output failure.
	localFlowMaxStdoutBytes = localFlowMaxOutputBytes + 64*1024
	localFlowMaxStderrBytes = 8 * 1024

	// The engine enforces its own 60 s execution timeout; the node bound adds
	// container start and stop.
	localFlowWallTimeout = 2 * time.Minute
	// localFlowUnusedGatewayPort is passed when the graph has no LocalInference
	// node. The CLI requires a syntactically valid port in every run, but a
	// deterministic run has no network at all, so nothing can dial it.
	localFlowUnusedGatewayPort = 9
	// localFlowRunRetention bounds the run-id ledger; a run id older than this
	// is no longer refused as a duplicate by this node.
	localFlowRunRetention = 30 * 24 * time.Hour

	localFlowContainerUser = "65532:65532"
	localFlowCPUs          = "1"
	localFlowMemory        = "1g"
	localFlowPids          = 256
	localFlowRunsTmpfs     = "type=tmpfs,target=/runs,tmpfs-size=67108864,tmpfs-mode=1777"
	localFlowTmpTmpfs      = "type=tmpfs,target=/tmp,tmpfs-size=16777216,tmpfs-mode=1777"
)

// localFlowPinnedImage is the digest-pinned engine image (repository@sha256:...).
// It stays empty until a release publishes and pins the aceteam#13130 image,
// which keeps every local-flow-run refused as "engine unavailable". The
// browserruntime.PinnedImage precedent: no node runs an image nobody pinned.
var localFlowPinnedImage = ""

// localFlowInputSchema, localFlowOutputSchema and localFlowRunner are the only
// manifest this node runs under the local-flow-run handler. The platform must
// register exactly this manifest; TestLocalFlowPinnedManifestHash pins its
// content hash against Python's compute_template_hash.
const (
	localFlowInputSchema = `{"type":"object","additionalProperties":false,` +
		`"required":["node_id","organization_id","flow_version_id","run_id","package_digest","graph_json","input_json"],` +
		`"properties":{` +
		`"node_id":{"type":"string","minLength":1,"maxLength":128},` +
		`"organization_id":{"type":"string","minLength":1,"maxLength":128},` +
		`"flow_version_id":{"type":"string","minLength":1,"maxLength":128,"pattern":"^[A-Za-z0-9][A-Za-z0-9._:-]*$"},` +
		`"run_id":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},` +
		`"package_digest":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},` +
		`"graph_json":{"type":"string","minLength":2,"maxLength":524288},` +
		`"input_json":{"type":"string","minLength":2,"maxLength":2097152}}}`
	localFlowOutputSchema = `{"type":"object","properties":{"result":{"type":"string","description":"result.json: the bounded Flow output and measured usage"}}}`
	localFlowRunner       = `{"kind":"builtin","handler":"local-flow-run"}`
)

var localFlowParamKeys = []string{"node_id", "organization_id", "flow_version_id", "run_id", "package_digest", "graph_json", "input_json"}

// localFlowNodeTypes is aceteam#13130's LOCAL_NODE_TYPES. The engine repeats a
// fuller admission check; the node refuses an obviously foreign graph first.
var localFlowNodeTypes = map[string]struct{}{
	"ConstantBoolean": {}, "ConstantInteger": {}, "ConstantString": {}, "Add": {}, "LocalInference": {},
}

// localFlowEngineErrors is the engine's fixed, non-secret error vocabulary.
// Anything else it prints is reported as "local execution failed".
var localFlowEngineErrors = map[string]struct{}{
	"invalid package": {}, "package too large": {}, "package digest mismatch": {},
	"unsupported graph": {}, "unsupported node": {}, "local models unavailable": {},
	"invalid local gateway": {}, "invalid graph": {}, "duplicate run": {},
	"local store unavailable": {}, "local execution failed": {}, "output too large": {},
}

var (
	localFlowRunIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	localFlowDigestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	localFlowImagePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	localFlowVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// errLocalFlowRefused marks a refusal before the engine started. It is
// terminal: the same request on the same node cannot become admissible.
var errLocalFlowRefused = errors.New("local-flow-run refused")

func localFlowRefuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errLocalFlowRefused, fmt.Sprintf(format, args...))
}

// localFlowPinnedContentHash is the approved manifest hash, computed once from
// the compiled-in manifest by the same function the worker uses to verify it.
var localFlowPinnedContentHash = func() string {
	h, err := jobs.ComputeTemplateManifestHash(localFlowTemplateKey, localFlowTemplateVersion,
		json.RawMessage(localFlowInputSchema), json.RawMessage(localFlowOutputSchema), json.RawMessage(localFlowRunner))
	if err != nil {
		panic(fmt.Sprintf("local-flow-run manifest does not canonicalize: %v", err))
	}
	return "sha256:" + h
}()

// localFlowDeps are the edges of the builtin. Production wiring is
// liveLocalFlowDeps; tests inject a fake container runtime and engine list.
type localFlowDeps struct {
	image   string
	runtime func() catalog.ContainerRuntime
	// engines lists the node's serving engines and models, as the gateway does.
	engines func(ctx context.Context) []gateway.ChatUpstream
	// preflightLimits proves cpu/memory/pids limits are enforceable.
	preflightLimits func(rt catalog.ContainerRuntime) error
	// stateDir is the run-id ledger directory (node config dir, not workspace).
	stateDir func() (string, error)
	// exec runs one engine command with optional stdin, capturing at most
	// stdoutLimit bytes of stdout. overflow reports a longer stdout.
	exec    func(cmd *exec.Cmd, stdin []byte, stdoutLimit int) (stdout []byte, overflow bool, err error)
	newName func() (string, error)
	now     func() time.Time
}

func init() {
	registerAttestedTemplateRunner(localFlowBuiltinName, func(ctx context.Context, bind templateRunBinding, params json.RawMessage, inputs []string, outDir string) (*attestedTemplateResult, error) {
		return runLocalFlowBuiltin(ctx, bind, params, inputs, outDir, localFlowDepsFactory())
	})
}

// localFlowDepsFactory is the production dependency source; tests replace it
// to drive the registered builtin through the live template adapter.
var localFlowDepsFactory = liveLocalFlowDeps

func liveLocalFlowDeps() localFlowDeps {
	return localFlowDeps{
		image:   localFlowPinnedImage,
		runtime: catalog.SelectContainerRuntime,
		engines: func(context.Context) []gateway.ChatUpstream { return newLocalChatLister()() },
		preflightLimits: func(rt catalog.ContainerRuntime) error {
			return rt.PreflightLimitControllers("cpu", "memory", "pids")
		},
		stateDir: func() (string, error) {
			dir := network.GetNodeConfigDir()
			if strings.TrimSpace(dir) == "" {
				return "", fmt.Errorf("node config directory is not resolved")
			}
			return filepath.Join(dir, "local-flow-runs"), nil
		},
		exec:    execLocalFlowCommand,
		newName: newLocalFlowContainerName,
		now:     time.Now,
	}
}

// localFlowBinding is hashed into the signed receipt's input_sha256. Every
// identity the run is bound to appears here; the platform recomputes the hash
// from the reported object with json.dumps(sort_keys=True, separators=(",",":")).
type localFlowBinding struct {
	Schema              string   `json:"schema"`
	TemplateKey         string   `json:"template_key"`
	TemplateVersion     int      `json:"template_version"`
	TemplateContentHash string   `json:"template_content_hash"`
	NodeID              string   `json:"node_id"`
	OrganizationID      string   `json:"organization_id"`
	JobID               string   `json:"job_id"`
	RunID               string   `json:"run_id"`
	FlowVersionID       string   `json:"flow_version_id"`
	PackageDigest       string   `json:"package_digest"`
	InputSHA256         string   `json:"input_sha256"`
	EngineImage         string   `json:"engine_image"`
	Network             string   `json:"network"`
	Models              []string `json:"models"`
}

type localFlowPlan struct {
	binding  localFlowBinding
	request  []byte
	port     int
	models   []string
	network  string
	runtime  catalog.ContainerRuntime
	ledger   string
	claimRun func() error
}

func runLocalFlowBuiltin(ctx context.Context, bind templateRunBinding, params json.RawMessage, inputs []string, outDir string, deps localFlowDeps) (*attestedTemplateResult, error) {
	ctx, cancel := context.WithTimeout(ctx, localFlowWallTimeout)
	defer cancel()

	plan, err := admitLocalFlowRun(ctx, bind, params, inputs, deps)
	if err != nil {
		return nil, err
	}
	// Hold the container ledger lock through reconciliation and execution so
	// another worker cannot mistake this run's live marker for stale cleanup.
	unlock, err := lockLocalFlowRunLedger(filepath.Join(plan.ledger, ".local-flow-containers.lock"))
	if err != nil {
		return nil, fmt.Errorf("local-flow-run: cannot lock container ledger: %w", err)
	}
	defer unlock()
	if err := reconcileLocalFlowContainers(plan, deps); err != nil {
		return nil, fmt.Errorf("local-flow-run: prior container cleanup is pending: %w", err)
	}
	// The ledger entry is the last step before launch: a refused run never
	// consumes its run id.
	if err := plan.claimRun(); err != nil {
		return nil, err
	}

	stdout, err := runLocalFlowContainer(ctx, plan, deps)
	if err != nil {
		return nil, err
	}
	output, usage, err := parseLocalFlowStdout(stdout)
	if err != nil {
		return nil, err
	}

	result := map[string]any{"output": output, "usage": usage}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("local-flow-run: encode result: %w", err)
	}
	canonical, err := jobs.NormalizeTemplateParams(raw)
	if err != nil {
		return nil, fmt.Errorf("local-flow-run: canonicalize result: %w", err)
	}
	if len(canonical) > localFlowMaxOutputBytes+1024 {
		return nil, fmt.Errorf("local-flow-run: engine output exceeds %d bytes", localFlowMaxOutputBytes)
	}
	resultPath := filepath.Join(outDir, localFlowResultFile)
	if err := removeStaleBuiltinOutput(resultPath); err != nil {
		return nil, err
	}
	if err := os.WriteFile(resultPath, append(canonical, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("local-flow-run: write result: %w", err)
	}

	usageReport := map[string]any{
		"units":  map[string]any{"local_flow_runs": int64(1)},
		"engine": "local_flow",
	}
	for k, v := range usage {
		usageReport[k] = v
	}
	in, inOK := usage["input_tokens"].(json.Number)
	outTok, outOK := usage["output_tokens"].(json.Number)
	usageReport["tokens_known"] = inOK && outOK
	if inOK {
		usageReport["prompt_tokens"] = in
	}
	if outOK {
		usageReport["completion_tokens"] = outTok
	}
	if inOK && outOK {
		a, errA := in.Int64()
		b, errB := outTok.Int64()
		if errA == nil && errB == nil {
			usageReport["total_tokens"] = a + b
		}
	}

	bindingSHA, err := canonicalJSONSHA256(plan.binding)
	if err != nil {
		return nil, fmt.Errorf("local-flow-run: hash binding: %w", err)
	}
	return &attestedTemplateResult{
		Outputs: []string{localFlowResultFile},
		Binding: plan.binding,
		Report: map[string]any{
			"output":         output,
			"usage":          usage,
			"binding":        plan.binding,
			"binding_sha256": bindingSHA,
		},
		Usage: usageReport,
	}, nil
}

// admitLocalFlowRun performs every refusal. Nothing it calls starts a
// container or writes node state; the returned plan's claimRun writes the
// ledger entry.
func admitLocalFlowRun(ctx context.Context, bind templateRunBinding, params json.RawMessage, inputs []string, deps localFlowDeps) (*localFlowPlan, error) {
	if bind.TemplateKey != localFlowTemplateKey || bind.TemplateVersion != localFlowTemplateVersion || bind.ContentHash != localFlowPinnedContentHash {
		return nil, localFlowRefuse("only the pinned %s v%d manifest %s runs this builtin", localFlowTemplateKey, localFlowTemplateVersion, localFlowPinnedContentHash)
	}
	if len(inputs) != 0 {
		return nil, localFlowRefuse("the template takes no input files")
	}
	if bind.NodeID == "" || bind.JobID == "" || bind.OrgID == "" {
		return nil, localFlowRefuse("node, job and organization identity are required")
	}
	m, err := decodeBuiltinParams(params, localFlowParamKeys...)
	if err != nil {
		return nil, localFlowRefuse("%v", err)
	}
	str := func(key string) (string, error) {
		s, ok := m[key].(string)
		if !ok || s == "" {
			return "", localFlowRefuse("param %q must be a non-empty string", key)
		}
		return s, nil
	}
	vals := make(map[string]string, len(localFlowParamKeys))
	for _, key := range localFlowParamKeys {
		s, err := str(key)
		if err != nil {
			return nil, err
		}
		vals[key] = s
	}
	if vals["node_id"] != bind.NodeID {
		return nil, localFlowRefuse("the request names a different node")
	}
	if vals["organization_id"] != bind.OrgID {
		return nil, localFlowRefuse("the request names a different organization than this node's stream")
	}
	if !localFlowVersionPattern.MatchString(vals["flow_version_id"]) {
		return nil, localFlowRefuse("flow_version_id is malformed")
	}
	if !localFlowRunIDPattern.MatchString(vals["run_id"]) {
		return nil, localFlowRefuse("run_id must be a canonical lowercase UUID")
	}
	if !localFlowDigestPattern.MatchString(vals["package_digest"]) {
		return nil, localFlowRefuse("package_digest is malformed")
	}

	graphCanon, graph, err := canonicalLocalFlowObject(vals["graph_json"], localFlowMaxGraphBytes, "graph")
	if err != nil {
		return nil, err
	}
	if got := "sha256:" + sha256Hex(string(graphCanon)); got != vals["package_digest"] {
		return nil, localFlowRefuse("the Flow package does not match its pinned digest")
	}
	inputCanon, _, err := canonicalLocalFlowObject(vals["input_json"], localFlowMaxInputBytes, "input")
	if err != nil {
		return nil, err
	}
	models, err := localFlowGraphModels(graph)
	if err != nil {
		return nil, err
	}

	netMode := "none"
	port := localFlowUnusedGatewayPort
	if len(models) > 0 {
		port, err = resolveLocalFlowModels(ctx, models, deps)
		if err != nil {
			return nil, err
		}
		// aceteam#13130's client dials http://127.0.0.1:<port>, so only the host
		// network namespace reaches the node's engine. Deterministic graphs keep
		// no network at all.
		netMode = "host"
	}

	request, err := buildLocalFlowRequest(graphCanon, inputCanon, vals["package_digest"], vals["run_id"])
	if err != nil {
		return nil, err
	}

	if deps.image == "" {
		return nil, localFlowRefuse("the local Flow engine is unavailable: no engine image is pinned in this release")
	}
	if !localFlowImagePattern.MatchString(deps.image) {
		return nil, localFlowRefuse("the local Flow engine is unavailable: the pinned image is not digest-pinned")
	}
	rt := deps.runtime()
	if err := localFlowImagePresent(ctx, rt, deps); err != nil {
		return nil, err
	}
	if err := deps.preflightLimits(rt); err != nil {
		return nil, localFlowRefuse("container limits cannot be enforced: %v", err)
	}

	ledger, err := deps.stateDir()
	if err != nil {
		return nil, localFlowRefuse("run ledger unavailable: %v", err)
	}
	if seen, err := localFlowRunSeen(ledger, vals["run_id"], deps.now(), localFlowRunRetention); err != nil {
		return nil, localFlowRefuse("run ledger unavailable: %v", err)
	} else if seen {
		return nil, localFlowRefuse("duplicate run: this node already ran %s", vals["run_id"])
	}

	runID := vals["run_id"]
	jobID := bind.JobID
	plan := &localFlowPlan{
		binding: localFlowBinding{
			Schema:              "citadel.local-flow-run.binding/v1",
			TemplateKey:         bind.TemplateKey,
			TemplateVersion:     bind.TemplateVersion,
			TemplateContentHash: bind.ContentHash,
			NodeID:              bind.NodeID,
			OrganizationID:      bind.OrgID,
			JobID:               bind.JobID,
			RunID:               runID,
			FlowVersionID:       vals["flow_version_id"],
			PackageDigest:       vals["package_digest"],
			InputSHA256:         "sha256:" + sha256Hex(string(inputCanon)),
			EngineImage:         deps.image,
			Network:             netMode,
			Models:              models,
		},
		request: request,
		port:    port,
		models:  models,
		network: netMode,
		runtime: rt,
		ledger:  ledger,
	}
	plan.claimRun = func() error {
		if err := claimLocalFlowRun(ledger, runID, jobID, deps.now(), localFlowRunRetention); err != nil {
			if errors.Is(err, os.ErrExist) {
				return localFlowRefuse("duplicate run: this node already ran %s", runID)
			}
			return localFlowRefuse("run ledger unavailable: %v", err)
		}
		return nil
	}
	return plan, nil
}

// canonicalLocalFlowObject decodes a JSON-string param with the template
// decoder (no duplicate keys, no lossy surrogates) and returns its
// Python-compatible canonical bytes, which are what the engine receives.
func canonicalLocalFlowObject(raw string, maxBytes int, name string) ([]byte, map[string]any, error) {
	canonical, err := jobs.NormalizeTemplateParams(json.RawMessage(raw))
	if err != nil {
		return nil, nil, localFlowRefuse("%s is not a single JSON object", name)
	}
	if len(canonical) > maxBytes {
		return nil, nil, localFlowRefuse("%s exceeds %d bytes", name, maxBytes)
	}
	v, err := jobs.DecodeTemplateJSON(canonical)
	if err != nil {
		return nil, nil, localFlowRefuse("%s is not a single JSON object", name)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, nil, localFlowRefuse("%s is not a single JSON object", name)
	}
	return canonical, obj, nil
}

// localFlowGraphModels checks the graph's top-level shape and node types and
// returns the sorted, unique models its LocalInference nodes name.
func localFlowGraphModels(graph map[string]any) ([]string, error) {
	if len(graph) != 4 {
		return nil, localFlowRefuse("unsupported graph")
	}
	for _, key := range []string{"input_node", "inner_nodes", "output_node", "edges"} {
		if _, ok := graph[key]; !ok {
			return nil, localFlowRefuse("unsupported graph")
		}
	}
	inner, ok := graph["inner_nodes"].([]any)
	if !ok || len(inner) > localFlowMaxInnerNodes {
		return nil, localFlowRefuse("unsupported graph")
	}
	if edges, ok := graph["edges"].([]any); !ok || len(edges) > localFlowMaxEdges {
		return nil, localFlowRefuse("unsupported graph")
	}
	seen := map[string]struct{}{}
	for _, n := range inner {
		node, ok := n.(map[string]any)
		if !ok {
			return nil, localFlowRefuse("unsupported graph")
		}
		typ, _ := node["type"].(string)
		if _, ok := localFlowNodeTypes[typ]; !ok {
			return nil, localFlowRefuse("unsupported node type in the Flow package")
		}
		if typ != "LocalInference" {
			continue
		}
		p, ok := node["params"].(map[string]any)
		if !ok {
			return nil, localFlowRefuse("unsupported graph")
		}
		model, ok := p["model"].(string)
		if !ok || model == "" || len(model) > 128 {
			return nil, localFlowRefuse("unsupported model")
		}
		seen[model] = struct{}{}
	}
	if len(seen) > localFlowMaxModels {
		return nil, localFlowRefuse("unsupported model")
	}
	models := make([]string, 0, len(seen))
	for m := range seen {
		models = append(models, m)
	}
	sort.Strings(models)
	return models, nil
}

// resolveLocalFlowModels requires every model to be served by this node, by
// exact name, on one engine port (the engine client dials a single port).
func resolveLocalFlowModels(ctx context.Context, models []string, deps localFlowDeps) (int, error) {
	if deps.engines == nil {
		return 0, localFlowRefuse("no local model engine is available on this node")
	}
	engines := deps.engines(ctx)
	port := 0
	for _, model := range models {
		found := 0
		for _, e := range engines {
			if e.Port <= 0 || e.Port > 65535 {
				continue
			}
			for _, served := range e.Models {
				if strings.TrimSpace(served) == model {
					found = e.Port
					break
				}
			}
			if found != 0 {
				break
			}
		}
		if found == 0 {
			return 0, localFlowRefuse("a LocalInference model is not served by a running engine on this node")
		}
		if port != 0 && port != found {
			return 0, localFlowRefuse("the Flow package's models are served by more than one engine")
		}
		port = found
	}
	return port, nil
}

func buildLocalFlowRequest(graphCanon, inputCanon []byte, digest, runID string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"graph":`)
	b.Write(graphCanon)
	b.WriteString(`,"input":`)
	b.Write(inputCanon)
	b.WriteString(`,"package_digest":"`)
	b.WriteString(digest)
	b.WriteString(`","run_id":"`)
	b.WriteString(runID)
	b.WriteString(`"}`)
	if b.Len() > localFlowMaxRequestBytes {
		return nil, localFlowRefuse("the Flow request exceeds %d bytes", localFlowMaxRequestBytes)
	}
	return b.Bytes(), nil
}

func localFlowImagePresent(ctx context.Context, rt catalog.ContainerRuntime, deps localFlowDeps) error {
	inspectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := rt.EngineCommandContext(inspectCtx, "image", "inspect", "--format", "{{.Id}}", deps.image)
	out, _, err := deps.exec(cmd, nil, 4096)
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return localFlowRefuse("the local Flow engine is unavailable: the pinned image is not present on this node")
	}
	return nil
}

// buildLocalFlowRunArgs returns the args after the engine binary. The image is
// last: no command or argument follows it, so the image's own entrypoint is
// the only thing that runs.
func buildLocalFlowRunArgs(plan *localFlowPlan, image, name string) ([]string, error) {
	models, err := json.Marshal(plan.models)
	if err != nil {
		return nil, err
	}
	if plan.models == nil {
		models = []byte("[]")
	}
	if plan.network != "none" && plan.network != "host" {
		return nil, fmt.Errorf("local-flow-run: unsupported network %q", plan.network)
	}
	return []string{
		"run", "--rm", "-i",
		"--name", name,
		"--pull", "never",
		"--network", plan.network,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--user", localFlowContainerUser,
		"--cpus", localFlowCPUs,
		"--memory", localFlowMemory,
		"--pids-limit", strconv.Itoa(localFlowPids),
		"--mount", localFlowRunsTmpfs,
		"--mount", localFlowTmpTmpfs,
		"-e", "CITADEL_LOCAL_RUNS_DIR=/runs",
		"-e", "CITADEL_LOCAL_GATEWAY_PORT=" + strconv.Itoa(plan.port),
		"-e", "CITADEL_LOCAL_MODELS=" + string(models),
		image,
	}, nil
}

func runLocalFlowContainer(ctx context.Context, plan *localFlowPlan, deps localFlowDeps) ([]byte, error) {
	name, err := deps.newName()
	if err != nil {
		return nil, err
	}
	if err := recordLocalFlowContainer(plan.ledger, name); err != nil {
		return nil, fmt.Errorf("local-flow-run: cannot track container before launch: %w", err)
	}
	args, err := buildLocalFlowRunArgs(plan, deps.image, name)
	if err != nil {
		_ = clearLocalFlowContainer(plan.ledger, name)
		return nil, err
	}
	// The engine CLI keeps the worker environment (rootless Podman needs its
	// runtime dir); the container itself receives only the three -e values.
	cmd := plan.runtime.EngineCommandContext(ctx, args...)
	cmd.WaitDelay = 5 * time.Second
	stdout, overflow, runErr := deps.exec(cmd, plan.request, localFlowMaxStdoutBytes)
	if runErr != nil || ctx.Err() != nil {
		// A killed engine client does not stop the container it started.
		if cleanupErr := removeLocalFlowContainer(plan.runtime, name, deps); cleanupErr != nil {
			// The durable marker remains for the next admission to reconcile.
			return nil, fmt.Errorf("local-flow-run: container outcome unknown; cleanup pending: %w", cleanupErr)
		}
	}
	if err := clearLocalFlowContainer(plan.ledger, name); err != nil {
		return nil, fmt.Errorf("local-flow-run: container cleanup marker remains: %w", err)
	}
	if overflow {
		return nil, fmt.Errorf("local-flow-run: engine output exceeds %d bytes", localFlowMaxStdoutBytes)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("local-flow-run: engine did not finish: %w", ctx.Err())
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		// Exit 1 with a fixed category is the engine's own refusal; parse it.
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 && len(bytes.TrimSpace(stdout)) > 0 {
			if _, _, err := parseLocalFlowStdout(stdout); err != nil {
				return nil, err
			}
		}
		code := -1
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		return nil, fmt.Errorf("local-flow-run: local Flow engine failed (exit %d)", code)
	}
	return stdout, nil
}

func removeLocalFlowContainer(rt catalog.ContainerRuntime, name string, deps localFlowDeps) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := deps.exec(rt.EngineCommandContext(ctx, "rm", "-f", name), nil, 4096)
	return err
}

var localFlowContainerNamePattern = regexp.MustCompile(`^citadel-local-flow-[a-z0-9-]{1,64}$`)

func localFlowContainerMarker(dir, name string) (string, error) {
	if !localFlowContainerNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid local Flow container name")
	}
	return filepath.Join(dir, ".cleanup-"+name), nil
}

func recordLocalFlowContainer(dir, name string) error {
	path, err := localFlowContainerMarker(dir, name)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close(), syncLocalFlowRunDir(dir))
}

func clearLocalFlowContainer(dir, name string) error {
	path, err := localFlowContainerMarker(dir, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncLocalFlowRunDir(dir)
}

func reconcileLocalFlowContainers(plan *localFlowPlan, deps localFlowDeps) error {
	entries, err := os.ReadDir(plan.ledger)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".cleanup-") {
			continue
		}
		name := strings.TrimPrefix(entry.Name(), ".cleanup-")
		if !entry.Type().IsRegular() || !localFlowContainerNamePattern.MatchString(name) {
			return fmt.Errorf("invalid cleanup marker")
		}
		if err := removeLocalFlowContainer(plan.runtime, name, deps); err != nil {
			return err
		}
		if err := clearLocalFlowContainer(plan.ledger, name); err != nil {
			return err
		}
	}
	return nil
}

// parseLocalFlowStdout accepts exactly {"output":{...},"usage":{"input_tokens":
// n|null,"output_tokens":n|null}} or {"error":"<fixed category>"}. A token
// count the engine could not measure stays null, never zero.
func parseLocalFlowStdout(stdout []byte) (map[string]any, map[string]any, error) {
	line := bytes.TrimSpace(stdout)
	if len(line) == 0 {
		return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned no result")
	}
	v, err := jobs.DecodeTemplateJSON(line)
	if err != nil {
		return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned a malformed result")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned a malformed result")
	}
	if len(m) == 1 {
		if category, ok := m["error"].(string); ok {
			if _, known := localFlowEngineErrors[category]; !known {
				category = "local execution failed"
			}
			return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine refused the run: %s", category)
		}
	}
	output, okOut := m["output"].(map[string]any)
	usage, okUsage := m["usage"].(map[string]any)
	if len(m) != 2 || !okOut || !okUsage || len(usage) != 2 {
		return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned a malformed result")
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		val, present := usage[key]
		if !present {
			return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned a malformed result")
		}
		if val == nil {
			continue
		}
		n, ok := val.(json.Number)
		if !ok {
			return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned a malformed result")
		}
		i, err := n.Int64()
		if err != nil || i < 0 {
			return nil, nil, fmt.Errorf("local-flow-run: the local Flow engine returned a malformed result")
		}
	}
	return output, usage, nil
}

// localFlowRunSeen is a read-only early refusal. Expiry and the final claim
// happen together under a process-shared lock in claimLocalFlowRun.
func localFlowRunSeen(dir, runID string, now time.Time, retention time.Duration) (bool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if info, err := root.Lstat(runID); err == nil {
		return !info.ModTime().Before(now.Add(-retention)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// claimLocalFlowRun prunes and claims under one cross-process lock. A stale
// preflight may never remove another worker's newly created claim.
func claimLocalFlowRun(dir, runID, jobID string, now time.Time, retention time.Duration) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockLocalFlowRunLedger(filepath.Join(dir, ".local-flow-runs.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cutoff := now.Add(-retention)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !localFlowRunIDPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if err := root.Remove(entry.Name()); err != nil {
				return err
			}
		}
	}
	f, err := root.OpenFile(runID, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, sanitizeTemplateSegment(jobID)+"\n")
	return errors.Join(writeErr, f.Sync(), f.Close(), syncLocalFlowRunDir(dir))
}

func newLocalFlowContainerName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("local-flow-run: allocate container name: %w", err)
	}
	return "citadel-local-flow-" + hex.EncodeToString(b[:]), nil
}

// localFlowCappedBuffer keeps the first limit bytes and drains the rest, so a
// chatty process can neither grow memory nor block on a full pipe.
type localFlowCappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *localFlowCappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.overflow = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.overflow = true
	}
	return len(p), nil
}

func execLocalFlowCommand(cmd *exec.Cmd, stdin []byte, stdoutLimit int) ([]byte, bool, error) {
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := &localFlowCappedBuffer{limit: stdoutLimit}
	stderr := &localFlowCappedBuffer{limit: localFlowMaxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	return stdout.buf.Bytes(), stdout.overflow, err
}
