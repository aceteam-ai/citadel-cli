package catalog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This is the CI guard for citadel-cli#1041: the ContainerRuntime seam. It fails
// when the number of LITERAL container-engine exec sites — exec.Command(...) or
// exec.CommandContext(...) whose command argument is a string literal "docker",
// "podman", "docker-compose", or "podman-compose" — EXCEEDS a baseline that can
// only shrink. Every such site should instead resolve a catalog.ContainerRuntime
// (catalog.SelectContainerRuntime()) and build the command through its
// EngineCommand / EngineCommandContext / ComposeCommand / ComposeCommandContext
// seam, so that podman nodes drive podman consistently.
//
// The scan is AST-based (not a regex) so it ignores comments and string
// constants, and treats exec.Command vs exec.CommandContext uniformly regardless
// of the ctx argument. It excludes _test.go files and the seam file itself
// (internal/catalog/runtime.go, whose two literal `podman` probes are the
// runtime-DETECTION primitives the seam is built on — they cannot themselves go
// through the seam).
//
// literalEngineExecBaseline is that ceiling. It started at 55 (the full literal
// inventory when the seam landed) and is lowered as sites are converted; the
// test prints every remaining site on each run so the constant is reconcilable
// in one step. It is deliberately `actual <= baseline` (not `==`): a follow-up
// may tighten it to `==` once the baseline reaches its irreducible carve-outs.
// Those carve-outs (why each remains) are documented in the PR and summarized in
// remainingCarveOuts below.
const literalEngineExecBaseline = 55

// remainingCarveOuts documents, by "path:function" hint, the sites intended to
// remain on the baseline after conversion and why. It is informational; the
// authoritative remaining list is whatever the scan prints on each run.
var remainingCarveOuts = []string{
	// import-graph cycle: internal/catalog imports internal/platform, so
	// internal/platform cannot import internal/catalog to reach the seam. Both
	// are `docker info` daemon-readiness probes (docker-specific by definition).
	"internal/platform/docker.go: WindowsDockerManager.Start (docker info readiness, x2)",
	// podman `manifest inspect` operates on manifest LISTS with different
	// semantics than docker's; converting needs separate compat validation.
	"internal/catalog/lockfile.go: resolveImageDigest (docker manifest inspect)",
	// docker-availability DETECTION primitives. Routing these through the
	// resolved runtime changes what "available" means per node; deferred with the
	// other detection carve-outs to keep this pass behavior-preserving.
	"internal/services/native.go: IsDockerAvailable (docker info)",
	"internal/jobs/service_payload.go: defaultDockerRuntimes (docker info --format Runtimes)",
	// raw `docker run` instance path (service_payload / storage gateway) builds a
	// docker-specific argv (runtime/network flags); podman-run compat is separate
	// follow-up work. Kept literal for now.
	"internal/jobs/service_payload.go: serviceStartPayload (docker run instance path)",
	"internal/storage/gateway.go: Start (docker run -d gateway path)",
}

var engineLiterals = map[string]bool{
	"docker":         true,
	"podman":         true,
	"docker-compose": true,
	"podman-compose": true,
}

// seamFileRelPath is excluded from the scan: it is where the seam and the
// runtime-detection probes live.
const seamFileRelPath = "internal/catalog/runtime.go"

type engineExecSite struct {
	relPath string
	line    int
	engine  string
}

func TestLiteralEngineExecBaseline(t *testing.T) {
	root := repoRoot(t)
	sites := scanLiteralEngineExecs(t, root)

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].relPath != sites[j].relPath {
			return sites[i].relPath < sites[j].relPath
		}
		return sites[i].line < sites[j].line
	})

	var b strings.Builder
	for _, s := range sites {
		b.WriteString("\n  " + s.relPath + ":" + strconv.Itoa(s.line) + " (" + s.engine + ")")
	}
	t.Logf("literal container-engine exec sites: %d (baseline %d)%s", len(sites), literalEngineExecBaseline, b.String())

	if len(sites) > literalEngineExecBaseline {
		t.Fatalf("literal container-engine exec sites = %d, exceeds baseline %d.\n"+
			"Route new engine execs through the catalog.ContainerRuntime seam "+
			"(EngineCommand/EngineCommandContext/ComposeCommand/ComposeCommandContext) "+
			"instead of exec.Command(\"docker\"/\"podman\", ...). Offending sites:%s",
			len(sites), literalEngineExecBaseline, b.String())
	}
	if len(sites) < literalEngineExecBaseline {
		t.Logf("literal engine exec sites (%d) is now BELOW the baseline (%d); "+
			"lower literalEngineExecBaseline to %d to keep the ratchet tight.",
			len(sites), literalEngineExecBaseline, len(sites))
	}
}

// repoRoot walks up from the test's working directory to the module root (the
// directory containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}

func scanLiteralEngineExecs(t *testing.T, root string) []engineExecSite {
	t.Helper()
	var sites []engineExecSite
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if rel == seamFileRelPath {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			// A file that does not parse cannot compile either; surface it rather
			// than silently under-counting.
			t.Fatalf("parse %s: %v", rel, parseErr)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			engine, isExec := literalEngineExecArg(call)
			if !isExec {
				return true
			}
			pos := fset.Position(call.Pos())
			sites = append(sites, engineExecSite{relPath: rel, line: pos.Line, engine: engine})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	return sites
}

// literalEngineExecArg reports whether call is exec.Command / exec.CommandContext
// whose command argument is a string literal in engineLiterals, and if so returns
// that literal.
func literalEngineExecArg(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return "", false
	}

	var cmdArgIdx int
	switch sel.Sel.Name {
	case "Command":
		cmdArgIdx = 0
	case "CommandContext":
		cmdArgIdx = 1 // args[0] is the context
	default:
		return "", false
	}
	if len(call.Args) <= cmdArgIdx {
		return "", false
	}
	lit, ok := call.Args[cmdArgIdx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	val, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	if engineLiterals[val] {
		return val, true
	}
	return "", false
}
