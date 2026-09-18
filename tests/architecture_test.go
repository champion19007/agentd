// Package tests holds cross-cutting tests that belong to no single package.
//
// The boundary test here is the executable form of the architecture rules. If
// someone reaches for a database handle inside the scheduler, this test fails
// before code review has to catch it.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/champion19007/agentd"

// coreRoot is the directory tree that must stay free of I/O, time and
// randomness.
const coreRoot = "../internal/core"

// forbiddenImports are import paths the core may never reach for, matched by
// exact path or by "prefix/" for a subtree. The core asks for these
// capabilities through internal/ports instead.
var forbiddenImports = []string{
	"net",
	"net/http",
	"os",
	"os/exec",
	"io/ioutil",
	"database/sql",
	"math/rand",
	"math/rand/v2",
	"crypto/rand",
	"modernc.org/sqlite",
	modulePath + "/internal/adapters",
	modulePath + "/internal/api",
	modulePath + "/internal/cli",
	modulePath + "/internal/config",
	modulePath + "/internal/plugins",
	modulePath + "/internal/store",
}

// forbiddenCalls are package-level functions the core may never call. The core
// imports "time" for durations and instants, but must not read the wall clock
// or block on it.
var forbiddenCalls = map[string][]string{
	"time": {"Now", "Sleep", "After", "Tick", "NewTimer", "NewTicker", "Since", "Until"},
}

func TestCoreDoesNotImportAdaptersOrIO(t *testing.T) {
	forEachCoreFile(t, func(t *testing.T, path string, f *ast.File) {
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquoting import %s: %v", path, spec.Path.Value, err)
			}
			for _, bad := range forbiddenImports {
				if imp == bad || strings.HasPrefix(imp, bad+"/") {
					t.Errorf("%s imports %q; the core reaches capabilities through internal/ports, not directly", path, imp)
				}
			}
		}
	})
}

func TestCoreDoesNotReadTheClock(t *testing.T) {
	forEachCoreFile(t, func(t *testing.T, path string, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fn := range forbiddenCalls[pkg.Name] {
				if sel.Sel.Name == fn {
					t.Errorf("%s calls %s.%s; the core takes time from ports.Clock", path, pkg.Name, fn)
				}
			}
			return true
		})
	})
}

// forEachCoreFile parses every non-test Go file under the core and hands it to
// check.
func forEachCoreFile(t *testing.T, check func(t *testing.T, path string, f *ast.File)) {
	t.Helper()

	fset := token.NewFileSet()
	err := filepath.WalkDir(coreRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		check(t, filepath.ToSlash(path), f)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", coreRoot, err)
	}
}
