package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestBuildSourcesPassesThePolicyToEveryConnector is a structural guard over
// internal/cli/app.go's buildSources.
//
// It exists because this exact wiring was missing once and nothing caught it.
// gmail.New / gchat.New / fastmail.New all take variadic Options and all
// default to policy.Default() when none is given, so omitting WithPolicy
// COMPILES, PASSES EVERY TEST, and silently archives under the built-in
// allowlist while ignoring the operator's whole [attachments] block. Worse,
// the notes and the skipped_attachments rows would record
// cfg.Policy().PolicyDigest() — a digest that did not produce those
// decisions — which is what `comms refetch` keys on, so the record would be
// actively wrong rather than merely stale.
//
// A behavioural test cannot reach this: buildSources needs live OAuth
// credentials, and the connectors keep their policy in an unexported field.
// So this asserts on the source text instead. It checks only the three
// connector constructors named below; a fourth connector must be added here
// by hand.
func TestBuildSourcesPassesThePolicyToEveryConnector(t *testing.T) {
	const file = "app.go"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	fn := findFunc(f, "buildSources")
	if fn == nil {
		t.Fatalf("%s: no buildSources function — this guard has drifted and must be updated", file)
	}

	connectors := map[string]bool{"gmail": false, "gchat": false, "fastmail": false}

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		pkg, sel, ok := pkgSelector(call.Fun)
		if !ok || sel != "New" {
			return true
		}
		if _, watched := connectors[pkg]; !watched {
			return true
		}
		for _, arg := range call.Args {
			argPkg, argSel, ok := pkgSelector(callee(arg))
			if ok && argPkg == pkg && argSel == "WithPolicy" {
				connectors[pkg] = true
				return true
			}
		}
		t.Errorf("%s:%d: %s.New is called without %s.WithPolicy(a.policy) — "+
			"the connector will fall back to policy.Default() and the operator's "+
			"[attachments] block will be silently ignored at sync time",
			file, fset.Position(call.Pos()).Line, pkg, pkg)
		return true
	})

	for pkg, seen := range connectors {
		if !seen {
			t.Errorf("%s: buildSources never constructs %s.New — this guard has drifted "+
				"and must be updated to match the real connector set", file, pkg)
		}
	}
}

// findFunc returns the top-level function declaration named name, or nil.
func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// pkgSelector destructures `pkg.Sel` into its two identifiers.
func pkgSelector(e ast.Expr) (pkg, sel string, ok bool) {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := s.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return id.Name, s.Sel.Name, true
}

// callee unwraps `f(...)` to f so an argument that is itself a call can be
// matched by name.
func callee(e ast.Expr) ast.Expr {
	if c, ok := e.(*ast.CallExpr); ok {
		return c.Fun
	}
	return e
}
