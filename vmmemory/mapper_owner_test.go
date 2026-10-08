package vmmemory_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The mapped record has one owner: what the bindings say is mapped changes in
// mapper.go, revocation.go and bindings.go alone, and every mapping command is
// sent from them (mapper.go). A path that set the record itself, before its
// command or after a refusal, is how three paths came to undo the wrong runs
// and one never to record at all; this keeps a new one from appearing.
func TestOnlyTheMapperRecordsWhatIsMapped(t *testing.T) {
	owners := []string{"mapper.go", "revocation.go", "bindings.go"}
	// The calls that send a mapping command or record its outcome.
	recorders := []string{"setMapped", "setBindingMapped", "mapZeros", "recordRun", "landed",
		"mapPages", "mapZeroPages", "mapBatch", "revokePage", "revokeBatch"}
	// The fields that say what is mapped.
	fields := []string{"mapped", "inZeroRun"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || slices.Contains(owners, path) {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.AssignStmt:
				for _, left := range n.Lhs {
					if sel, ok := left.(*ast.SelectorExpr); ok && slices.Contains(fields, sel.Sel.Name) {
						t.Errorf("%s writes .%s: only the mapper records what is mapped", fset.Position(n.Pos()), sel.Sel.Name)
					}
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if slices.Contains(recorders, sel.Sel.Name) {
					t.Errorf("%s calls %s: only the mapper sends mapping commands and records them",
						fset.Position(n.Pos()), sel.Sel.Name)
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "mapping" &&
					slices.Contains([]string{"Map", "MapZero", "Revoke", "MapBatch", "RevokeBatch"}, sel.Sel.Name) {
					t.Errorf("%s calls mapping.%s: only the mapper sends mapping commands", fset.Position(n.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}
