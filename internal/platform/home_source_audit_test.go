package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Inspect selector expressions rather than just calls: a package-level saved
// os.UserHomeDir function value also bypasses a later fixture installation.
func rawHomeSelectors(file *ast.File, allowHomeDir bool) (bad []token.Pos, allowed int) {
	aliases := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Path.Value != `"os"` {
			continue
		}
		name := "os"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			bad = append(bad, imp.Pos()) // A dot import obscures the provenance.
		}
		aliases[name] = true
	}
	for _, decl := range file.Decls {
		resolver := false
		if fn, ok := decl.(*ast.FuncDecl); ok {
			resolver = allowHomeDir && fn.Recv == nil && fn.Name.Name == "HomeDir"
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "UserHomeDir" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || !aliases[id.Name] {
				return true
			}
			if resolver {
				allowed++
			} else {
				bad = append(bad, sel.Pos())
			}
			return true
		})
	}
	return bad, allowed
}

type homeSourceAudit struct {
	files, allowed int
	bypasses       []token.Position
	byTopLevel     map[string]int
}

func auditHomeSources(root string) (homeSourceAudit, error) {
	result := homeSourceAudit{byTopLevel: map[string]int{}}
	resolver := filepath.Join(root, "internal", "platform", "platform.go")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "testdata" || entry.Name() == "vendor" || entry.Name() == "node_modules" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result.files++
		result.byTopLevel[strings.Split(filepath.ToSlash(rel), "/")[0]]++
		bad, n := rawHomeSelectors(file, path == resolver)
		result.allowed += n
		for _, pos := range bad {
			result.bypasses = append(result.bypasses, set.Position(pos))
		}
		return nil
	})
	return result, err
}

func TestRepositoryHomeCallersUseFixtureResolver(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("repository root not found:", err)
	}
	result, err := auditHomeSources(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, pos := range result.bypasses {
		t.Errorf("%s: use platform.UserHomeDir, not the raw OS resolver", pos)
	}
	for _, name := range []string{"cmd", "internal", "services"} {
		if result.byTopLevel[name] == 0 {
			t.Errorf("audit did not inspect %s production files", name)
		}
	}
	if result.files == 0 || result.allowed != 1 {
		t.Fatalf("audit must inspect repository source and exactly one central OS fallback: files=%d, fallbacks=%d", result.files, result.allowed)
	}
}

func TestHomeSourceAuditWalksRootAndTopLevelPackages(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"internal/platform/platform.go": `package platform; import "os"; func HomeDir() { os.UserHomeDir() }`,
		"services/ports.go":             `package services; import sys "os"; var home = sys.UserHomeDir`,
		"cmd/tool.go":                   `package cmd; import "os"; func f() { os.UserHomeDir() }`,
		"tools.go":                      `package tools; import "os"; func HomeDir() { os.UserHomeDir() }`,
		"newpackage/home.go":            `package newpackage; import "os"; var home = os.UserHomeDir`,
		"services/fixture_test.go":      `package services; import "os"; var fixture = os.UserHomeDir`,
		"testdata/invalid.go":           "not Go source",
		"vendor/invalid.go":             "not Go source",
		"node_modules/invalid.go":       "not Go source",
		".git/invalid.go":               "not Go source",
	}
	for rel, source := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := auditHomeSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.files != 5 || result.allowed != 1 || len(result.bypasses) != 4 {
		t.Fatalf("top-level audit coverage lost: files=%d fallback=%d bypasses=%d", result.files, result.allowed, len(result.bypasses))
	}
	for _, name := range []string{"internal", "services", "cmd", "tools.go", "newpackage"} {
		if result.byTopLevel[name] != 1 {
			t.Fatalf("audit did not inspect %s exactly once", name)
		}
	}
	// Syntax errors in included source must fail rather than disappear from
	// the audit. Only owned synthetic files are ever written here.
	if err := os.WriteFile(filepath.Join(root, "services", "broken.go"), []byte("not Go source"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := auditHomeSources(root); err == nil {
		t.Fatal("audit ignored an included source parse error")
	}
}

func TestHomeSourceAuditCatchesCallsAliasesAndSavedFunctions(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		central      bool
		bad, allowed int
	}{
		{"call", `package p; import "os"; func f() { os.UserHomeDir() }`, false, 1, 0},
		{"saved alias", `package p; import sys "os"; var home = sys.UserHomeDir`, false, 1, 0},
		{"dot import", `package p; import . "os"; var home = UserHomeDir`, false, 1, 0},
		{"other central function", `package p; import "os"; func other() { os.UserHomeDir() }`, true, 1, 0},
		{"central fallback", `package p; import "os"; func HomeDir() { os.UserHomeDir() }`, true, 0, 1},
		{"shim", `package p; import "example/platform"; var home = platform.UserHomeDir`, false, 0, 0},
		{"text only", `package p; const note = "os.UserHomeDir"`, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tc.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			bad, allowed := rawHomeSelectors(file, tc.central)
			if len(bad) != tc.bad || allowed != tc.allowed {
				t.Fatalf("audit result = (%d, %d), want (%d, %d)", len(bad), allowed, tc.bad, tc.allowed)
			}
		})
	}
}
