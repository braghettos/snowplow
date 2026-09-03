package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// F10 — wiring guard, AST-structural (1.12.4 design §4.3, gate condition C6).
//
// F6 proves the decorator. This proves its INSTALLATION: main() must obtain
// its slog.Handler by calling buildLogHandler, and must not construct a
// slog.NewJSONHandler / slog.NewTextHandler itself (which would bypass the
// trace-correlation decorator). It is a structural assertion over main.go's
// AST, not a hand-maintained list of forbidden constructions.
//
// RED on main (8de5295): main() builds the handler inline with an if/else
// over slog.NewTextHandler / slog.NewJSONHandler and buildLogHandler does
// not exist.
func TestF10_MainInstallsHandlerViaBuildLogHandler(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var mainFn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "main" && fd.Recv == nil {
			mainFn = fd
		}
	}
	if mainFn == nil {
		t.Fatal("func main not found")
	}

	calls := map[string]int{} // "buildLogHandler", "slog.NewJSONHandler", "slog.NewTextHandler"
	var slogNewArgIsBuild bool
	ast.Inspect(mainFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "buildLogHandler" {
				calls["buildLogHandler"]++
			}
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "slog" {
				switch fn.Sel.Name {
				case "NewJSONHandler", "NewTextHandler":
					calls["slog."+fn.Sel.Name]++
				case "New":
					// slog.New(lh): lh must be the identifier assigned from
					// buildLogHandler. Resolve one level: either the arg is
					// the call itself, or an identifier whose assignment is
					// a buildLogHandler call.
					if len(call.Args) == 1 {
						slogNewArgIsBuild = argComesFromBuildLogHandler(mainFn.Body, call.Args[0])
					}
				}
			}
		}
		return true
	})

	if calls["buildLogHandler"] != 1 {
		t.Fatalf("main() must call buildLogHandler exactly once, found %d", calls["buildLogHandler"])
	}
	if n := calls["slog.NewJSONHandler"] + calls["slog.NewTextHandler"]; n != 0 {
		t.Fatalf("main() constructs %d base slog handler(s) inline — that bypasses the trace-correlation decorator; use buildLogHandler", n)
	}
	if !slogNewArgIsBuild {
		t.Fatal("slog.New(...) in main() is not fed by the buildLogHandler result")
	}
}

// argComesFromBuildLogHandler reports whether expr is a direct call to
// buildLogHandler or an identifier whose (single) assignment in body is such
// a call.
func argComesFromBuildLogHandler(body *ast.BlockStmt, expr ast.Expr) bool {
	if isBuildCall(expr) {
		return true
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			if l, ok := lhs.(*ast.Ident); ok && l.Name == id.Name && i < len(as.Rhs) && isBuildCall(as.Rhs[i]) {
				found = true
			}
		}
		return true
	})
	return found
}

func isBuildCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	return ok && fn.Name == "buildLogHandler"
}
