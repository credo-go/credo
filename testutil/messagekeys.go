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
// and for each key the check cannot read from the source.
//
// catalogDir has the layout [credo.I18nConfig.Dir] loads,
// {catalogDir}/{lang}/messages.json; a key is defined when any language's
// messages.json has it. The sources are parsed, not compiled: a key is found
// where it is written as a literal —
//
//   - the first argument of a call of a method named T or TPlural
//     ([credo.Context.T], [credo.Context.TPlural]),
//   - the argument of WithMessageKey or RenderMessageKey,
//   - the value given to a MessageKey field, in a composite literal or an
//     assignment,
//   - a constant or variable declared with type MessageKey,
//   - a conversion to MessageKey.
//
// An argument that names a constant declared with type MessageKey, in any
// package under sourceRoots, is checked where the constant is declared.
//
// A key built at run time is a family: a concatenation that begins with a
// literal, or with a string constant declared under sourceRoots, whose text
// ends in "." —
//
//	c.T(credo.MessageKey("status." + code))
//
// The check cannot know the computed part, so it proves what it can: it fails
// when no catalog defines a key that begins with the prefix. Leading operands
// that are all literals and string constants are joined into the prefix, and
// a concatenation made of nothing else is one key, checked as a literal is.
//
// A constant is read by its name, since the sources are not type-checked: a
// constant declared in the function that uses it, else one declared at
// package level in the package of the file, or in the package the file
// imports. A name the function also declares as something else — a variable,
// a parameter, a type — anywhere in its body is not read, so a prefix
// constant and a local of the same name do not share a function; nor is a
// name declared with different values in one package (in files for different
// platforms), or by two directories that match the import path equally well.
//
// Any other value is reported: the check cannot see it, so the key cannot be
// proved to be in the catalogs. That covers a key computed with no literal
// prefix, a prefix that does not end in ".", a variable or a parameter, and a
// key that reaches a call through a constant declared without the MessageKey
// type, so declare key constants with their type. Test files, testdata,
// vendor and hidden directories are skipped.
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
	var files []sourceFile
	for _, root := range sourceRoots {
		parsed, err := parseSources(fset, root)
		if err != nil {
			tb.Fatalf("testutil.CheckMessageKeys: %v", err)
			return
		}
		files = append(files, parsed...)
	}

	s := &keyScan{fset: fset, typedConsts: map[string]bool{}, consts: map[constName]*stringConst{}}
	for _, f := range files {
		s.collectConsts(f)
	}
	for _, f := range files {
		s.scan(f)
	}
	for _, k := range s.keys {
		if !defined[k.value] {
			tb.Errorf("%s: message key %q is not defined in the catalogs under %s", k.pos, k.value, catalogDir)
		}
	}
	for _, f := range s.families {
		if !hasKeyUnder(defined, f.value) {
			tb.Errorf("%s: message key family %q, the literal prefix of %q, has no key in the catalogs under %s; add a key that begins with %q to a catalog, or correct the prefix",
				f.pos, f.value, f.expr, catalogDir, f.value)
		}
	}
	for _, n := range s.nonLiteral {
		tb.Errorf("%s: message key expression %q is not a literal and does not begin with a literal or a string constant that ends in \".\", so the check cannot prove it is in the catalogs; write the key as a literal or a constant of type MessageKey, or build it from a literal prefix that ends in \".\" and the computed part",
			n.pos, n.value)
	}
}

// hasKeyUnder reports whether a defined key begins with prefix and continues
// after it.
func hasKeyUnder(defined map[string]bool, prefix string) bool {
	for k := range defined {
		if len(k) > len(prefix) && strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
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

// sourceFile is a parsed file and the directory of its package.
type sourceFile struct {
	dir  string
	file *ast.File
}

func parseSources(fset *token.FileSet, root string) ([]sourceFile, error) {
	var files []sourceFile
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
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return err
		}
		files = append(files, sourceFile{dir: dir, file: f})
		return nil
	})
	return files, err
}

type foundKey struct {
	pos   token.Position
	value string
	expr  string // the expression a family's prefix was read from
}

// constName names a constant by the directory of its package and its name.
type constName struct {
	dir  string
	name string
}

// stringConst is a constant's value expression, the file that declares it
// and, for a constant declared in a function, that function's names. A name
// declared more than once with different expressions — at package level in
// files for different platforms, or twice in one function — is ambiguous,
// and is not read.
type stringConst struct {
	value     ast.Expr
	in        sourceFile
	scope     *localNames
	ambiguous bool
}

// localNames are the names a top-level declaration declares inside itself:
// in the body of a function, or in the function literals of a variable's
// value. The sources are not type-checked, so the names of every block are
// taken together: a name declared in any of them hides the package's.
type localNames struct {
	consts map[string]*stringConst
	other  map[string]bool // variables, parameters, results, receivers, types
}

func (l *localNames) declares(name string) bool {
	return l != nil && (l.other[name] || l.consts[name] != nil)
}

// addConst records a constant; one written without a value of its own (it
// repeats the previous expression) is not read.
func (l *localNames) addConst(vs *ast.ValueSpec, in sourceFile) {
	for i, n := range vs.Names {
		if len(vs.Values) != len(vs.Names) {
			l.other[n.Name] = true
			continue
		}
		if prev := l.consts[n.Name]; prev != nil {
			prev.ambiguous = prev.ambiguous || types.ExprString(prev.value) != types.ExprString(vs.Values[i])
			continue
		}
		l.consts[n.Name] = &stringConst{value: vs.Values[i], in: in, scope: l}
	}
}

func (l *localNames) addFields(lists ...*ast.FieldList) {
	for _, list := range lists {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, n := range field.Names {
				l.other[n.Name] = true
			}
		}
	}
}

func (l *localNames) addDefined(exprs ...ast.Expr) {
	for _, e := range exprs {
		if id, ok := e.(*ast.Ident); ok {
			l.other[id.Name] = true
		}
	}
}

// localNamesOf collects the names decl declares inside itself. The names a
// top-level const, var or type declaration gives the package are not local.
func localNamesOf(decl ast.Decl, in sourceFile) *localNames {
	l := &localNames{consts: map[string]*stringConst{}, other: map[string]bool{}}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			l.addFields(n.Recv, n.Type.TypeParams, n.Type.Params, n.Type.Results)
		case *ast.FuncLit:
			l.addFields(n.Type.Params, n.Type.Results)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				l.addDefined(n.Lhs...)
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				l.addDefined(n.Key, n.Value)
			}
		case *ast.GenDecl:
			if ast.Node(n) == ast.Node(decl) {
				return true
			}
			for _, spec := range n.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					if n.Tok == token.CONST {
						l.addConst(spec, in)
						continue
					}
					for _, name := range spec.Names {
						l.other[name.Name] = true
					}
				case *ast.TypeSpec:
					l.other[spec.Name.Name] = true
				}
			}
		}
		return true
	})
	return l
}

type keyScan struct {
	fset        *token.FileSet
	typedConsts map[string]bool // names of constants declared with type MessageKey
	consts      map[constName]*stringConst
	file        sourceFile  // the file being scanned
	locals      *localNames // the names of the top-level declaration being scanned
	keys        []foundKey
	families    []foundKey
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

// collectConsts records the constants of f: those declared with type
// MessageKey by name, wherever they are declared, and every package-level
// constant with a value by package and name, for a family's prefix.
func (s *keyScan) collectConsts(f sourceFile) {
	ast.Inspect(f.file, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if vs.Type != nil && isKeyType(vs.Type) {
				for _, n := range vs.Names {
					s.typedConsts[n.Name] = true
				}
			}
		}
		return true
	})
	for _, decl := range f.file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Values) != len(vs.Names) {
				continue
			}
			for i, n := range vs.Names {
				name := constName{dir: f.dir, name: n.Name}
				if prev, dup := s.consts[name]; dup {
					prev.ambiguous = prev.ambiguous || types.ExprString(prev.value) != types.ExprString(vs.Values[i])
					continue
				}
				s.consts[name] = &stringConst{value: vs.Values[i], in: f}
			}
		}
	}
}

func (s *keyScan) scan(f sourceFile) {
	s.file = f
	for _, decl := range f.file.Decls {
		s.locals = localNamesOf(decl, f)
		s.scanDecl(decl)
	}
}

func (s *keyScan) scanDecl(decl ast.Decl) {
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			if n.Type != nil && isKeyType(n.Type) {
				for _, v := range n.Values {
					s.value(v)
				}
			}
		case *ast.CallExpr:
			switch fun := n.Fun.(type) {
			case *ast.Ident:
				if len(n.Args) == 1 && isKeyCall(fun.Name) {
					s.value(n.Args[0])
				}
			case *ast.SelectorExpr:
				switch name := fun.Sel.Name; {
				case len(n.Args) == 1 && isKeyCall(name),
					name == "T" && len(n.Args) >= 1,
					name == "TPlural" && len(n.Args) >= 2:
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

// isKeyCall reports whether name is a function whose one argument is a key.
func isKeyCall(name string) bool {
	return name == "RenderMessageKey" || name == "WithMessageKey" || name == "MessageKey"
}

// value records a key written as expr: a literal is a key to check; a typed
// key constant and a conversion are checked where they are written; a
// concatenation that begins with constant text ending in "." is a family;
// anything else is not a literal.
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
	case *ast.BinaryExpr:
		prefix, whole := s.constPrefix(e)
		switch {
		case whole && prefix != "":
			s.keys = append(s.keys, foundKey{pos: pos, value: prefix})
			return
		case !whole && strings.HasSuffix(prefix, "."):
			s.families = append(s.families, foundKey{pos: pos, value: prefix, expr: types.ExprString(expr)})
			return
		}
	}
	s.nonLiteral = append(s.nonLiteral, foundKey{pos: pos, value: types.ExprString(expr)})
}

// constPrefix joins the leading operands of the concatenation e that are
// literals or string constants, and reports whether every operand is one.
func (s *keyScan) constPrefix(e *ast.BinaryExpr) (prefix string, whole bool) {
	var b strings.Builder
	for _, operand := range concatOperands(e, nil) {
		v, ok := s.constString(operand, s.file, s.locals, 0)
		if !ok {
			return b.String(), false
		}
		b.WriteString(v)
	}
	return b.String(), true
}

// concatOperands appends the operands of a chain of + in source order.
func concatOperands(expr ast.Expr, operands []ast.Expr) []ast.Expr {
	if b, ok := ast.Unparen(expr).(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return concatOperands(b.Y, concatOperands(b.X, operands))
	}
	return append(operands, expr)
}

// constString returns the text of expr when it is a string literal, a
// constant declared under the source roots whose value is one, or a
// concatenation of those. expr is read as written in file, inside the
// declaration whose names are scope (nil at package level): a name the
// declaration gives a variable, a parameter or a type is not a constant.
func (s *keyScan) constString(expr ast.Expr, file sourceFile, scope *localNames, depth int) (string, bool) {
	if depth > 16 { // a declaration cycle does not compile; do not follow one
		return "", false
	}
	switch e := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			v, err := strconv.Unquote(e.Value)
			return v, err == nil
		}
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := s.constString(e.X, file, scope, depth+1)
		if !ok {
			return "", false
		}
		y, ok := s.constString(e.Y, file, scope, depth+1)
		return x + y, ok
	case *ast.Ident:
		if scope.declares(e.Name) {
			return s.constValue(scope.consts[e.Name], depth)
		}
		return s.constValue(s.consts[constName{dir: file.dir, name: e.Name}], depth)
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok || scope.declares(pkg.Name) {
			return "", false
		}
		return s.importedConst(file, pkg.Name, e.Sel.Name, depth)
	}
	return "", false
}

// constValue reads a constant from its declaration; c is nil for a name that
// is not a constant.
func (s *keyScan) constValue(c *stringConst, depth int) (string, bool) {
	if c == nil || c.ambiguous {
		return "", false
	}
	return s.constString(c.value, c.in, c.scope, depth+1)
}

// importedConst reads the constant name of the package file imports as pkg.
// The sources are not compiled, so the package is found by its directory:
// the one under the source roots whose path ends with the most elements of
// the import path. The directories are ranked first and only the best are
// read, so the answer does not depend on the order they are met in; two
// that rank alike are ambiguous unless both are read as the same text.
func (s *keyScan) importedConst(file sourceFile, pkg, name string, depth int) (string, bool) {
	var importPath string
	for _, imp := range file.file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == pkg {
			importPath = p
			break
		}
	}
	if importPath == "" {
		return "", false
	}
	var (
		best   int
		atBest []*stringConst
	)
	for c, decl := range s.consts {
		if c.name != name {
			continue
		}
		n := commonSuffixElements(filepath.ToSlash(c.dir), importPath)
		if n == 0 || n < best {
			continue
		}
		if n > best {
			best, atBest = n, atBest[:0]
		}
		atBest = append(atBest, decl)
	}
	if len(atBest) == 0 {
		return "", false
	}
	value, ok := s.constValue(atBest[0], depth)
	for _, decl := range atBest[1:] {
		v, readable := s.constValue(decl, depth)
		ok = ok && readable && v == value
	}
	if !ok {
		return "", false
	}
	return value, true
}

// commonSuffixElements counts the trailing path elements a and b share.
func commonSuffixElements(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[len(as)-1-n] == bs[len(bs)-1-n] {
		n++
	}
	return n
}
