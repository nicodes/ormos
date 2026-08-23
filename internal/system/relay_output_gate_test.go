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

// TestRelayDerivedOutputGate is deliberately narrower than a general taint
// analyzer. It follows the relay-derived values in this package that have a
// terminal-facing sink: the device-flow error returned to runSystem, the port
// poll error echoed by logf in headless mode, and the two headless pairing
// fields. It also pins the one explicit formatting allowlist: the relay-chosen
// registered system name may bypass sanitizeRelayOutput only through %q,
// whose Go-syntax quoting escapes terminal controls.
//
// This inspects every sink in the relevant reachable blocks, rather than
// asserting that one approved symbol still exists. Adding another raw print in
// one of those blocks therefore fails the gate.
func TestRelayDerivedOutputGate(t *testing.T) {
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, name := range []string{"login.go", "run.go", "system.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("relay output gate: parse %s: %v", name, err)
		}
		files[name] = f
	}

	// The error variable initialized by performLogin is relay-tainted. Its
	// lexical scope is the if body, so inspect every output call in that body.
	runSystem := gateFunc(t, files["run.go"], "runSystem")
	foundLoginBranch := false
	ast.Inspect(runSystem.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || !gateContainsCall(stmt.Body, "performLogin") {
			return true
		}
		foundLoginBranch = true
		gateInspectCalls(t, fset, stmt.Body, func(call *ast.CallExpr) {
			if !gateCallHasIdent(call, "err") {
				return
			}
			if gateCallName(call) == "writeLoginError" {
				return
			}
			if gateIsTerminalSink(call) && !gateTaintedArgsSafe(call, "err") {
				gateUnsafe(t, fset, call, "runSystem", "err")
			}
		})
		return false
	})
	if !foundLoginBranch {
		t.Fatal("relay output gate: runSystem no longer has a performLogin branch to inspect")
	}

	// Verify the approved helper itself sanitizes the error at its writer.
	writeLoginError := gateFunc(t, files["run.go"], "writeLoginError")
	gateInspectCalls(t, fset, writeLoginError.Body, func(call *ast.CallExpr) {
		if gateIsPrintCall(call) && gateCallHasIdent(call, "err") && !gateTaintedArgsSafe(call, "err") {
			gateUnsafe(t, fset, call, "writeLoginError", "err")
		}
	})

	// fetchConfiguredPorts' err is relay-tainted when pollPorts logs it. Scan
	// every log sink in pollPorts, including its nested tick closure.
	pollPorts := gateFunc(t, files["system.go"], "pollPorts")
	gateInspectCalls(t, fset, pollPorts.Body, func(call *ast.CallExpr) {
		if gateCallName(call) == "logf" && gateCallHasIdent(call, "err") && !gateTaintedArgsSafe(call, "err") {
			gateUnsafe(t, fset, call, "pollPorts", "err")
		}
	})

	// The headless pairing output consumes relay response fields directly.
	headless := gateFunc(t, files["login.go"], "headlessCodeDisplay")
	for _, field := range []string{"UserCode", "VerificationURL"} {
		gateInspectCalls(t, fset, headless.Body, func(call *ast.CallExpr) {
			if gateIsPrintCall(call) && gateCallHasSelector(call, "s", field) &&
				!gateTaintedSelectorSafe(call, "s", field) {
				gateUnsafe(t, fset, call, "headlessCodeDisplay", "s."+field)
			}
		})
	}

	// A future direct diagnostic inside the relay decoder is a new output path,
	// not an exception. e, resp, data and s are the response-derived locals in
	// the non-200 branch.
	postRelayJSON := gateFunc(t, files["login.go"], "postRelayJSON")
	gateInspectCalls(t, fset, postRelayJSON.Body, func(call *ast.CallExpr) {
		if !gateIsTerminalSink(call) {
			return
		}
		for _, source := range []string{"e", "resp", "data", "s"} {
			if gateCallHasIdent(call, source) && !gateTaintedArgsSafe(call, source) {
				gateUnsafe(t, fset, call, "postRelayJSON", source)
			}
		}
	})

	// Explicit, single-site allowlist control. If this changes from %q to %s or
	// %v, the relay-selected name becomes a raw terminal string and must fail.
	performLogin := gateFunc(t, files["login.go"], "performLogin")
	foundQuotedName := false
	gateInspectCalls(t, fset, performLogin.Body, func(call *ast.CallExpr) {
		if !gateCallHasSelector(call, "out", "Name") {
			return
		}
		foundQuotedName = true
		if !gateIsTerminalSink(call) || !strings.Contains(gateFormat(call), "%q") {
			gateUnsafe(t, fset, call, "performLogin", "out.Name (only %q is allowlisted)")
		}
	})
	if !foundQuotedName {
		t.Fatal("relay output gate: performLogin's relay-derived out.Name output is no longer present to verify")
	}
}

func gateFunc(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("relay output gate: function %s not found", name)
	return nil
}

func gateInspectCalls(t *testing.T, fset *token.FileSet, node ast.Node, check func(*ast.CallExpr)) {
	t.Helper()
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			check(call)
		}
		return true
	})
}

func gateContainsCall(node ast.Node, name string) bool {
	if node == nil {
		return false
	}
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && gateCallName(call) == name {
			found = true
			return false
		}
		return true
	})
	return found
}

func gateCallName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}

func gateIsPrintCall(call *ast.CallExpr) bool {
	name := gateCallName(call)
	return strings.HasPrefix(name, "Fprint") || strings.HasPrefix(name, "Print")
}

func gateIsTerminalSink(call *ast.CallExpr) bool {
	if gateCallName(call) == "logf" {
		return true
	}
	if !gateIsPrintCall(call) || len(call.Args) == 0 {
		return false
	}
	selector, ok := call.Args[0].(*ast.SelectorExpr)
	root, rootOK := selector.X.(*ast.Ident)
	return ok && rootOK && root.Name == "os" && selector.Sel.Name == "Stderr"
}

func gateCallHasIdent(call *ast.CallExpr, name string) bool {
	return gateNodeHasIdent(call, name)
}

func gateCallHasSelector(call *ast.CallExpr, root, field string) bool {
	return gateNodeHasSelector(call, root, field)
}

func gateNodeHasSelector(node ast.Node, root, field string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		id, idOK := func() (*ast.Ident, bool) {
			if !ok {
				return nil, false
			}
			id, ok := sel.X.(*ast.Ident)
			return id, ok
		}()
		if idOK && id.Name == root && sel.Sel.Name == field {
			found = true
			return false
		}
		return true
	})
	return found
}

func gateNodeHasIdent(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

func gateTaintedArgsSafe(call *ast.CallExpr, ident string) bool {
	for _, arg := range call.Args {
		if gateNodeHasIdent(arg, ident) && !gateSanitized(arg, ident) {
			return false
		}
	}
	return true
}

func gateTaintedSelectorSafe(call *ast.CallExpr, root, field string) bool {
	for _, arg := range call.Args {
		if gateNodeHasSelector(arg, root, field) && !gateSanitizedSelector(arg, root, field) {
			return false
		}
	}
	return true
}

func gateSanitized(expr ast.Expr, ident string) bool {
	call, ok := expr.(*ast.CallExpr)
	return ok && gateCallName(call) == "sanitizeRelayOutput" && gateNodeHasIdent(call, ident)
}

func gateSanitizedSelector(expr ast.Expr, root, field string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || (gateCallName(call) != "sanitize" && gateCallName(call) != "sanitizeRelayOutput") {
		return false
	}
	return gateCallHasSelector(call, root, field)
}

func gateFormat(call *ast.CallExpr) string {
	index := 0
	if gateIsPrintCall(call) && len(call.Args) > 1 {
		index = 1 // fmt.Fprintf(writer, format, ...)
	}
	if len(call.Args) <= index {
		return ""
	}
	lit, ok := call.Args[index].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return value
}

func gateUnsafe(t *testing.T, fset *token.FileSet, call *ast.CallExpr, function, value string) {
	t.Helper()
	pos := fset.Position(call.Pos())
	t.Errorf("relay output gate: %s:%d: %s relay-derived %s reaches a terminal output without sanitizeRelayOutput or the explicit %%q allowlist",
		pos.Filename, pos.Line, function, value)
}
