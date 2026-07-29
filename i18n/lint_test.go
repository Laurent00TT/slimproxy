package i18n

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// TestEveryChineseStringIsTranslated is the safety net the inline T(zh, en)
// form depends on.
//
// A message catalog fails a missed translation at extraction time; inline
// bilingualism has no extraction step, so a hardcoded Chinese string simply
// ships -- and appears, untranslated, in the middle of an English session.
// This test walks every non-test source file in the module and fails on any
// Chinese string literal that is not the first argument of an i18n.T or
// i18n.NewError call.
//
// Only the first argument: T("中文", "english with 中文 quoted") is legal --
// the English translation of a message ABOUT Chinese text may quote it -- but
// a Chinese string anywhere else is a message the English interface will leak.
func TestEveryChineseStringIsTranslated(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Skip everything that is not this module's production code.
			if name == ".git" || name == "_backup_20260726-ui-removal" || name == "logs" ||
				name == "auths" || name == "deploy" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}

		// Collect the positions of Chinese literals that are legitimately
		// Chinese: first arguments to i18n.T / i18n.NewError (bare T /
		// NewError inside this package itself), and values of struct fields
		// whose name ends in "Zh" -- the convention init-time registries use
		// when they cannot call T yet (see the CLI's summaryZh/summaryEn pair).
		sanctioned := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if len(node.Args) == 0 || !isTranslationCall(node.Fun) {
					return true
				}
				if lit, ok := node.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					sanctioned[lit.Pos()] = true
				}
			case *ast.KeyValueExpr:
				key, ok := node.Key.(*ast.Ident)
				if !ok || !strings.HasSuffix(key.Name, "Zh") {
					return true
				}
				if lit, ok := node.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					sanctioned[lit.Pos()] = true
				}
			}
			return true
		})

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !containsHan(lit.Value) {
				return true
			}
			if sanctioned[lit.Pos()] {
				return true
			}
			pos := fset.Position(lit.Pos())
			rel, _ := filepath.Rel(root, pos.Filename)
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, pos.Line, truncateLit(lit.Value)))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("发现 %d 处不在 i18n.T 里的中文字符串——英文界面会原样漏出这些内容:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func isTranslationCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		id, ok := f.X.(*ast.Ident)
		return ok && id.Name == "i18n" && (f.Sel.Name == "T" || f.Sel.Name == "NewError")
	case *ast.Ident:
		// Inside package i18n itself.
		return f.Name == "T" || f.Name == "NewError"
	}
	return false
}

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func truncateLit(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// moduleRoot walks up from the working directory to go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}
