/*
Copyright 2026 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package system

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestSystemWindowsSingleRunnerAndLogOrdering(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "system_windows.go", nil, 0)
	if err != nil {
		t.Fatalf("parser.ParseFile(\"system_windows.go\") failed: %v", err)
	}

	var runCalls, execCalls, execCmdCalls int
	funcs := map[string]*ast.FuncDecl{}

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			funcs[fn.Name.Name] = fn
		}
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
		switch pkgIdent.Name + "." + sel.Sel.Name {
		case "goolib.Run":
			runCalls++
		case "goolib.Exec":
			execCalls++
		case "exec.Command":
			execCmdCalls++
		}
		return true
	})

	if runCalls != 2 {
		t.Errorf("system_windows.go goolib.Run call count = %d, want 2", runCalls)
	}
	if execCalls != 0 {
		t.Errorf("system_windows.go goolib.Exec call count = %d, want 0", execCalls)
	}
	if execCmdCalls != 0 {
		t.Errorf("system_windows.go exec.Command call count = %d, want 0", execCmdCalls)
	}

	// Verify that both InstallWith and UninstallWith create the log file before
	// calling buildCmd (which invokes goolib.Command and can fail on unknown
	// script extensions), and call goolib.Run using buildCmd's command.
	for _, fnName := range []string{"InstallWith", "UninstallWith"} {
		fn, ok := funcs[fnName]
		if !ok {
			t.Fatalf("system_windows.go missing function %s", fnName)
		}
		var createPos, buildPos, runPos token.Pos
		var fnRuns int
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "buildCmd" && buildPos == token.NoPos {
					buildPos = call.Pos()
				}
			case *ast.SelectorExpr:
				if pkg, ok := fun.X.(*ast.Ident); ok {
					switch pkg.Name + "." + fun.Sel.Name {
					case "oswrap.Create":
						if createPos == token.NoPos {
							createPos = call.Pos()
						}
					case "goolib.Run":
						fnRuns++
						if runPos == token.NoPos {
							runPos = call.Pos()
						}
					}
				}
			}
			return true
		})

		if fnRuns != 1 {
			t.Errorf("%s goolib.Run call count = %d, want 1", fnName, fnRuns)
		}
		if createPos == token.NoPos || buildPos == token.NoPos || runPos == token.NoPos {
			t.Fatalf("%s call positions create=%v buildCmd=%v goolib.Run=%v, want all present", fnName, createPos, buildPos, runPos)
		}
		if createPos >= buildPos {
			t.Errorf("%s calls oswrap.Create (pos %d) after buildCmd (pos %d), want log file created before buildCmd", fnName, createPos, buildPos)
		}
		if buildPos >= runPos {
			t.Errorf("%s calls buildCmd (pos %d) after goolib.Run (pos %d), want buildCmd before goolib.Run", fnName, buildPos, runPos)
		}
	}
}
