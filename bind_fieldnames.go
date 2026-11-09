package credo

import (
	"reflect"
	"strings"
	"sync"

	"github.com/credo-go/credo/validation"
)

// The validation engine names a violation's field by its json tag, because
// Validate takes no binding source. A bind from a form or a query renames the
// fields of the violations its target's Validate returns to the tags of that
// source, so a violation names the field as the client sent it. The rename
// walks the target's type along the field path; it runs only when validation
// fails, and the per-type tables are cached.

// sourceField is a struct field as a binding source names it: its name in
// that source and its type, for the rest of the path.
type sourceField struct {
	name string
	typ  reflect.Type
}

// sourceFieldsKey identifies a (struct type, source tag) pair.
type sourceFieldsKey struct {
	typ reflect.Type
	tag string
}

// sourceFieldsCache maps each struct field's json-derived name, the name the
// validation engine reports, to the field as a source tag names it.
// Key: sourceFieldsKey, Value: map[string]sourceField
var sourceFieldsCache sync.Map

// sourceFieldNames renames the fields of err's violations, when err is the
// validation.Errors or *validation.ValidationError a target's Validate
// returned, from the json names the engine reports to the names the source
// tag gives. tag "json" keeps the engine's names, and any other error is
// returned unchanged.
func sourceFieldNames(err error, target any, tag string) error {
	if err == nil || tag == "json" {
		return err
	}
	t := reflect.TypeOf(target)
	switch e := err.(type) { //nolint:errorlint // Only the error Validate returned itself can be rebuilt with new names.
	case validation.Errors:
		out := make(validation.Errors, len(e))
		for i, ve := range e {
			ve.Field = sourceFieldPath(t, ve.Field, tag)
			out[i] = ve
		}
		return out
	case *validation.ValidationError:
		if e == nil {
			return err
		}
		ve := *e
		ve.Field = sourceFieldPath(t, ve.Field, tag)
		return &ve
	}
	return err
}

// sourceFieldPath renames each name segment of path, a field path such as
// items[0].name, that names a field of the type reached so far. A segment
// past an interface, a map key or a name no field has is kept as it is.
func sourceFieldPath(t reflect.Type, path, tag string) string {
	var b strings.Builder
	b.Grow(len(path))
	typ := t
	for i := 0; i < len(path); {
		switch path[i] {
		case '.':
			b.WriteByte('.')
			i++
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				b.WriteString(path[i:])
				return b.String()
			}
			b.WriteString(path[i : i+end+1])
			i += end + 1
			typ = elementType(typ)
		default:
			end := strings.IndexAny(path[i:], ".[")
			if end < 0 {
				end = len(path) - i
			}
			segment := path[i : i+end]
			i += end
			name, next := segment, reflect.Type(nil)
			if st := structType(typ); st != nil {
				if f, ok := sourceFields(st, tag)[segment]; ok {
					name, next = f.name, f.typ
				}
			}
			b.WriteString(name)
			typ = next
		}
	}
	return b.String()
}

// structType returns t, or what its pointers point to, when that is a struct.
func structType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// elementType returns the element type of a slice or array, through pointers,
// or nil for any other type.
func elementType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
		return nil
	}
	return t.Elem()
}

func sourceFields(t reflect.Type, tag string) map[string]sourceField {
	key := sourceFieldsKey{typ: t, tag: tag}
	if cached, ok := sourceFieldsCache.Load(key); ok {
		return cached.(map[string]sourceField)
	}
	m := make(map[string]sourceField, t.NumField())
	buildSourceFields(t, tag, m, map[reflect.Type]bool{})
	sourceFieldsCache.Store(key, m)
	return m
}

// buildSourceFields walks t's exported fields in declaration order, embedded
// structs included, as the validation engine does when it names them: the
// first field of a json-derived name wins.
func buildSourceFields(t reflect.Type, tag string, m map[string]sourceField, seen map[reflect.Type]bool) {
	if seen[t] {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if engineName := tagFieldName(f, "json"); m[engineName].typ == nil {
			m[engineName] = sourceField{name: tagFieldName(f, tag), typ: f.Type}
		}
		if f.Anonymous {
			if st := structType(f.Type); st != nil {
				buildSourceFields(st, tag, m, seen)
			}
		}
	}
}

// tagFieldName is the name tag gives f: the tag's first comma-separated token,
// or the Go field name when the tag is absent, "-" or has no name. It is the
// validation engine's rule for the json tag, applied to any tag.
func tagFieldName(f reflect.StructField, tag string) string {
	value := f.Tag.Get(tag)
	if value != "" && value != "-" {
		if name, _, _ := strings.Cut(value, ","); name != "" {
			return name
		}
	}
	return f.Name
}
