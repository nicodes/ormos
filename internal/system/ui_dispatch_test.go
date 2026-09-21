//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// `komizo ui` shipped unreachable once: the command existed in one switch and
// not in the argv router's second copy of the same list, and nothing compared
// them. This file is ormos's version of that guard, one level up: the case is
// read from the SOURCE (parsed, not grepped, so a comment cannot satisfy it),
// and then every name is DRIVEN through Main, because a source agreement the
// runtime does not honour is the same bug one floor higher.

func TestMainHasAUICaseThatCallsTheSeam(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Main" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			namesUI := false
			for _, expr := range cc.List {
				ast.Inspect(expr, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok {
						if v, err := strconv.Unquote(lit.Value); err == nil && v == "ui" {
							namesUI = true
						}
					}
					return true
				})
			}
			if !namesUI {
				return true
			}
			for _, stmt := range cc.Body {
				ast.Inspect(stmt, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name == "uiMainFn" {
						found = true
					}
					return true
				})
			}
			return true
		})
	}
	if !found {
		t.Fatal("Main has no case naming \"ui\" that reaches uiMainFn -- the command would ship unreachable")
	}
}

func TestMainDrivesUIThroughTheSeam(t *testing.T) {
	oldUI, oldRun := uiMainFn, runSystemFn
	t.Cleanup(func() { uiMainFn, runSystemFn = oldUI, oldRun })
	var gotArgs []string
	var gotVersion string
	uiMainFn = func(args []string, version string) error {
		gotArgs, gotVersion = append([]string{}, args...), version
		return nil
	}
	ran := false
	runSystemFn = func() { ran = true }

	Main([]string{"ui"}, "9.9.9")
	if gotVersion != "9.9.9" {
		t.Fatalf("version did not reach the UI: %q", gotVersion)
	}
	if len(gotArgs) != 0 {
		t.Fatalf("ui got extra args: %v", gotArgs)
	}
	if ran {
		t.Fatal("ormos ui must not also run the tunnel")
	}
}

func TestUsageNamesTheUI(t *testing.T) {
	if !strings.Contains(usageText, "ormos ui") {
		t.Fatal("usage() does not name `ormos ui`")
	}
}
