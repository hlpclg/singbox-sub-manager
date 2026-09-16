package backup

// TestStaticCheck_NoForbiddenPathBasedCalls implements design §7.2's
// source-level check: outside the one whitelisted exception (design §6.2's
// openTrustedRoot, which resolves a trusted root by following symlinks,
// exactly like v0.7, via os.MkdirAll), internal/backup must not call any of
// the path-based functions design §6.3 and §6.6 replace with fd-relative
// equivalents — a call site reaching one of them either re-resolves a name
// by path (defeating the fd-relative migration this whole version exists
// for) or sets permissions/ownership by name (design §6.3's explicit
// prohibition, since Fchmodat/Fchownat follow a symlink unless the
// fchmodat2 variant is used, which needs a newer kernel than this project
// supports).
//
// This check parses each non-test .go file's AST (comments and string
// literals — including ones that happen to spell out a forbidden call, as
// in this file's own doc comments — never look like a call to go/ast) and
// flags every os.*/unix.* selector call against the forbidden list below,
// unless the specific (file, "pkg.Selector") pair is in the whitelist. Per
// design §7.2, a clean result here is not evidence of safety by itself —
// it only stops new code from silently reintroducing a path-based call.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenCalls is design §7.2's exact list, split by the package
// identifier the call is written through in this codebase.
var forbiddenCalls = map[string]map[string]bool{
	"os": {
		"Rename":     true,
		"Remove":     true,
		"RemoveAll":  true,
		"MkdirAll":   true,
		"Mkdir":      true,
		"CreateTemp": true,
		"Lstat":      true,
		"ReadFile":   true,
		"Chmod":      true,
		"Chown":      true,
	},
	"unix": {
		"Fchmodat": true,
		"Fchownat": true,
	},
}

// staticCheckWhitelist names the exact (file, "pkg.Selector") pairs design
// §6.2 permits: only openTrustedRoot's own os.MkdirAll, which resolves the
// trusted root itself by following symlinks. Any other forbidden call
// anywhere in dirfd.go — or anywhere else in the package — still fails.
var staticCheckWhitelist = map[string]map[string]bool{
	"dirfd.go": {"os.MkdirAll": true},
}

func TestStaticCheck_NoForbiddenPathBasedCalls(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if !forbiddenCalls[pkgIdent.Name][sel.Sel.Name] {
				return true
			}
			qualified := pkgIdent.Name + "." + sel.Sel.Name
			if staticCheckWhitelist[name][qualified] {
				return true
			}
			pos := fset.Position(call.Pos())
			t.Errorf("%s:%d: forbidden call %s (design §7.2's whitelist covers only dirfd.go's os.MkdirAll in openTrustedRoot)",
				filepath.Base(pos.Filename), pos.Line, qualified)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no non-test .go files were found to check — the check itself is broken")
	}
}
