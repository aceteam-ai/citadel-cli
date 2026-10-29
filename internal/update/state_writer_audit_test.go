package update

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// A locked SaveState cannot fix a snapshot loaded before the lock was taken.
// Production consumers must use MutateState or the explicit preference setter.
func TestProductionStateConsumersUseTransactions(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository fixture sources")
	}
	root := filepath.Join(filepath.Dir(source), "..", "..")
	for _, directory := range []string{"cmd", "internal", "services"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			aliases := map[string]bool{}
			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return err
				}
				if name == "github.com/aceteam-ai/citadel-cli/internal/update" {
					alias := "update"
					if imported.Name != nil {
						alias = imported.Name.Name
					}
					if alias == "." {
						t.Errorf("%s: updater state imports must be explicit", path)
					}
					aliases[alias] = true
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "SaveState" {
					return true
				}
				if identifier, ok := selector.X.(*ast.Ident); ok && aliases[identifier.Name] {
					t.Errorf("%s: full snapshot SaveState can clobber a concurrent preference; use a transaction", path)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
