package testutil

import (
	jsonv2 "encoding/json/v2"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CheckMessageKeys fails tb for each message key in the Go sources under
// sourceRoots that the translation catalogs under catalogDir do not define,
// and for each key whose value is not a literal.
//
// catalogDir has the layout [credo.I18nConfig.Dir] loads,
// {catalogDir}/{lang}/messages.json; a key is defined when any language's
// messages.json has it. The sources are parsed, not compiled: a key is found
// where it is written as a literal —
//
//   - the argument of WithMessageKey or RenderMessageKey,
//   - the value given to a MessageKey field, in a composite literal or an
//     assignment,
//   - a constant or variable declared with type MessageKey,
//   - a conversion to MessageKey.
//
// An argument that names a constant declared with type MessageKey, in any
// package under sourceRoots, is checked where the constant is declared. Any
// other value that is not a literal is reported: the check cannot see it, so a
// key computed at run time cannot be proved to be in the catalogs. A key that
// reaches WithMessageKey through an untyped constant is outside the scan, so
// declare key constants with their type. Test files, testdata, vendor and
// hidden directories are skipped.
//
// The application chooses its catalog, so the check belongs in the
// application's tests:
//
//	func TestMessageKeys(t *testing.T) {
//		testutil.CheckMessageKeys(t, "locales", ".")
//	}
func CheckMessageKeys(tb testing.TB, catalogDir string, sourceRoots ...string) {
	tb.Helper()
	defined, err := catalogKeys(catalogDir)
	if err != nil {
		tb.Fatalf("testutil.CheckMessageKeys: %v", err)
		return
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, root := range sourceRoots {
		parsed, err := parseSources(fset, root)
		if err != nil {
			tb.Fatalf("testutil.CheckMessageKeys: %v", err)
			return
		}
		files = append(files, parsed...)
	}

	s := &keyScan{fset: fset, typedConsts: map[string]bool{}}
	for _, f := range files {
		s.collectTypedConsts(f)
	}
	for _, f := range files {
		s.scan(f)
	}
	for _, k := range s.keys {
		if !defined[k.value] {
			tb.Errorf("%s: message key %q is not defined in the catalogs under %s", k.pos, k.value, catalogDir)
		}
	}
	for _, n := range s.nonLiteral {
		tb.Errorf("%s: message key expression %q is not a literal, so the check cannot prove it is in the catalogs", n.pos, n.value)
	}
}

// catalogKeys returns the union of the keys of every language's
// messages.json under dir.
func catalogKeys(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "messages.json")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var raw map[string]any
		if err := jsonv2.Unmarshal(data, &raw); err != nil {
			return nil, &fs.PathError{Op: "parse", Path: path, Err: err}
		}
		for k := range raw {
			keys[k] = true
		}
		found = true
	}
	if !found {
		return nil, &fs.PathError{Op: "read", Path: dir, Err: fs.ErrNotExist}
	}
	return keys, nil
}

func parseSources(fset *token.FileSet, root string) ([]*ast.File, error) {
	var files []*ast.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	return files, err
}

type foundKey struct {
	pos   token.Position
	value string
}

type keyScan struct {
	fset        *token.FileSet
	typedConsts map[string]bool // names of constants declared with type MessageKey
	keys        []foundKey
	nonLiteral  []foundKey
}

// isKeyType reports whether expr names the MessageKey type: MessageKey,
// credo.MessageKey or validation.MessageKey.
func isKeyType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "MessageKey"
	case *ast.SelectorExpr:
		return e.Sel.Name == "MessageKey"
	}
	return false
}

func (s *keyScan) collectTypedConsts(f *ast.File) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if vs.Type == nil || !isKeyType(vs.Type) {
				continue
			}
			for _, n := range vs.Names {
				s.typedConsts[n.Name] = true
			}
		}
	}
}

func (s *keyScan) scan(f *ast.File) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			if n.Type != nil && isKeyType(n.Type) {
				for _, v := range n.Values {
					s.value(v)
				}
			}
		case *ast.CallExpr:
			if len(n.Args) != 1 {
				return true
			}
			switch fun := n.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "RenderMessageKey" || fun.Name == "WithMessageKey" || fun.Name == "MessageKey" {
					s.value(n.Args[0])
				}
			case *ast.SelectorExpr:
				if fun.Sel.Name == "RenderMessageKey" || fun.Sel.Name == "WithMessageKey" || fun.Sel.Name == "MessageKey" {
					s.value(n.Args[0])
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok && id.Name == "MessageKey" {
				s.value(n.Value)
			}
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				return true
			}
			for i, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "MessageKey" {
					s.value(n.Rhs[i])
				}
			}
		}
		return true
	})
}

// value records a key written as expr: a literal is a key to check; a typed
// key constant and a conversion are checked where they are written; anything
// else is not a literal.
func (s *keyScan) value(expr ast.Expr) {
	pos := s.fset.Position(expr.Pos())
	switch e := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if v, err := strconv.Unquote(e.Value); err == nil {
				if v != "" {
					s.keys = append(s.keys, foundKey{pos: pos, value: v})
				}
				return
			}
		}
	case *ast.Ident:
		if s.typedConsts[e.Name] {
			return
		}
	case *ast.SelectorExpr:
		if s.typedConsts[e.Sel.Name] {
			return
		}
	case *ast.CallExpr:
		if len(e.Args) == 1 && isKeyType(e.Fun) {
			return // the conversion is scanned as a call of its own
		}
	}
	s.nonLiteral = append(s.nonLiteral, foundKey{pos: pos, value: types.ExprString(expr)})
}
