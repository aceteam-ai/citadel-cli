package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

func TestInternalHomeCallersUseFixtureResolver(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	resolver := filepath.Join(root, "platform", "platform.go")
	files, allowed := 0, 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "testdata" || entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
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
		files++
		bad, n := rawHomeSelectors(file, path == resolver)
		allowed += n
		for _, pos := range bad {
			t.Errorf("%s: use platform.UserHomeDir, not the raw OS resolver", set.Position(pos))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 || allowed != 1 {
		t.Fatalf("audit must inspect internal source and exactly one central OS fallback: files=%d, fallbacks=%d", files, allowed)
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
