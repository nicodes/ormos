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

	// The performLogin branch is the bounded live-terminal region. Reject every
	// dynamic terminal write there unless it uses the approved writer or is
	// independently safe. This deliberately does not depend on the current
	// variable name: `relayErr := err` must not evade the gate.
	runSystem := gateFunc(t, files["run.go"], "runSystem")
	foundLoginBranch := false
	ast.Inspect(runSystem.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || !gateContainsCall(stmt.Body, "performLogin") {
			return true
		}
		foundLoginBranch = true
		gateInspectCalls(t, fset, stmt.Body, func(call *ast.CallExpr) {
			if gateCallName(call) == "writeLoginError" {
				return
			}
			if gateIsTerminalSink(call) && !gateSinkArgsSafe(call) {
				gateUnsafe(t, fset, call, "runSystem", "dynamic value in the performLogin branch")
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
		if gateIsPrintCall(call) && !gateSinkArgsSafe(call) {
			gateUnsafe(t, fset, call, "writeLoginError", "dynamic value")
		}
	})

	// pollPorts is the bounded headless-echo region. As above, inspect every log
	// sink rather than only calls that retain the current identifier `err`.
	pollPorts := gateFunc(t, files["system.go"], "pollPorts")
	gateInspectCalls(t, fset, pollPorts.Body, func(call *ast.CallExpr) {
		if gateCallName(call) == "logf" && !gateSinkArgsSafe(call) {
			gateUnsafe(t, fset, call, "pollPorts", "dynamic value")
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
	// not an exception. Reject any unsafe dynamic terminal sink in this bounded
	// producer regardless of how a response-derived value was renamed.
	postRelayJSON := gateFunc(t, files["login.go"], "postRelayJSON")
	gateInspectCalls(t, fset, postRelayJSON.Body, func(call *ast.CallExpr) {
		if gateIsTerminalSink(call) && !gateSinkArgsSafe(call) {
			gateUnsafe(t, fset, call, "postRelayJSON", "dynamic value")
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
		if !gateIsTerminalSink(call) || gateSelectorVerb(call, "out", "Name") != 'q' {
			gateUnsafe(t, fset, call, "performLogin", "out.Name (only %q is allowlisted)")
		}
	})
	if !foundQuotedName {
		t.Fatal("relay output gate: performLogin's relay-derived out.Name output is no longer present to verify")
	}
}

func TestGatePercentQAppliesToTheRelayArgument(t *testing.T) {
	for _, tc := range []struct {
		name, expression string
		want             rune
	}{
		{"direct q", `fmt.Fprintf(os.Stderr, "name %q", out.Name)`, 'q'},
		{"q on later literal", `fmt.Fprintf(os.Stderr, "name %s (%q)", out.Name, "control")`, 's'},
		{"escaped percent q", `fmt.Fprintf(os.Stderr, "name %s (%%q)", out.Name)`, 's'},
		{"q on the second argument", `fmt.Fprintf(os.Stderr, "control %s; name %q", "local", out.Name)`, 'q'},
		{"explicit index rejected conservatively", `fmt.Fprintf(os.Stderr, "name %[1]q", out.Name)`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := parser.ParseExpr(tc.expression)
			if err != nil {
				t.Fatal(err)
			}
			call := expr.(*ast.CallExpr)
			if got := gateSelectorVerb(call, "out", "Name"); got != tc.want {
				t.Errorf("gateSelectorVerb(%s) = %q, want %q", tc.expression, got, tc.want)
			}
		})
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

func gateTaintedSelectorSafe(call *ast.CallExpr, root, field string) bool {
	for i, arg := range call.Args {
		if gateNodeHasSelector(arg, root, field) && !gateSanitizedSelector(arg, root, field) {
			return gateArgumentVerb(call, i) == 'q'
		}
	}
	return true
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

// gateSinkArgsSafe accepts only literals, direct sanitizeRelayOutput wrappers,
// or a %q that consumes the same argument. It is intentionally conservative
// within the two small in-scope blocks; a new known-safe dynamic value needs an
// explicit, reviewable exception rather than silently expanding the allowlist.
func gateSinkArgsSafe(call *ast.CallExpr) bool {
	start := gateDataArgStart(call)
	if start < 0 {
		return false
	}
	for i := start; i < len(call.Args); i++ {
		arg := call.Args[i]
		if _, literal := arg.(*ast.BasicLit); literal {
			continue
		}
		if sanitized, ok := arg.(*ast.CallExpr); ok && gateCallName(sanitized) == "sanitizeRelayOutput" {
			continue
		}
		if gateArgumentVerb(call, i) == 'q' {
			continue
		}
		return false
	}
	return true
}

func gateSelectorVerb(call *ast.CallExpr, root, field string) rune {
	for i, arg := range call.Args {
		if gateNodeHasSelector(arg, root, field) {
			return gateArgumentVerb(call, i)
		}
	}
	return 0
}

func gateDataArgStart(call *ast.CallExpr) int {
	name := gateCallName(call)
	switch {
	case name == "logf":
		return 1
	case name == "Fprintf":
		return 2
	case name == "Printf":
		return 1
	case name == "Fprintln" || name == "Fprint":
		return 1
	case name == "Println" || name == "Print":
		return 0
	default:
		return -1
	}
}

// gateArgumentVerb returns the format verb that consumes call.Args[argIndex].
// The allowlist rejects formats with explicit indexes or star operands rather
// than risk mapping them incorrectly; no in-scope approved site needs either.
func gateArgumentVerb(call *ast.CallExpr, argIndex int) rune {
	start := gateDataArgStart(call)
	if start < 0 || argIndex < start {
		return 0
	}
	verbs, ok := gateFormatVerbs(gateFormat(call))
	if !ok || argIndex-start >= len(verbs) {
		return 0
	}
	return verbs[argIndex-start]
}

func gateFormatVerbs(format string) ([]rune, bool) {
	runes := []rune(format)
	var verbs []rune
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' {
			continue
		}
		i++
		if i >= len(runes) {
			return nil, false
		}
		if runes[i] == '%' {
			continue
		}
		for i < len(runes) && strings.ContainsRune("+#- 0", runes[i]) {
			i++
		}
		if i < len(runes) && (runes[i] == '[' || runes[i] == '*') {
			return nil, false
		}
		for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
			i++
		}
		if i < len(runes) && runes[i] == '.' {
			i++
			if i < len(runes) && (runes[i] == '[' || runes[i] == '*') {
				return nil, false
			}
			for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
				i++
			}
		}
		if i >= len(runes) || runes[i] == '[' || runes[i] == '*' || runes[i] == '%' {
			return nil, false
		}
		verbs = append(verbs, runes[i])
	}
	return verbs, true
}

func gateUnsafe(t *testing.T, fset *token.FileSet, call *ast.CallExpr, function, value string) {
	t.Helper()
	pos := fset.Position(call.Pos())
	t.Errorf("relay output gate: %s:%d: %s relay-derived %s reaches a terminal output without sanitizeRelayOutput or the explicit %%q allowlist",
		pos.Filename, pos.Line, function, value)
}
