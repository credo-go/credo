package credo

import (
	"encoding"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// defaultMultipartMaxMemory is the default maximum memory (in bytes) used
// for parsing multipart form data. Matches Go stdlib's default of 32 MB.
const defaultMultipartMaxMemory = 32 << 20

// multipartFileHeaderPtrType is a cached reflect.Type used by bindMultipartFiles
// to identify file fields (*multipart.FileHeader and []*multipart.FileHeader).
var multipartFileHeaderPtrType = reflect.TypeOf((*multipart.FileHeader)(nil))

// stringSliceType is the fast-path type for plain []string fields.
var stringSliceType = reflect.TypeOf([]string(nil))

// tagField maps a struct tag value to a field index and kind.
// Used by decodeValues to decode url.Values (query params, form data,
// multipart values) into struct fields.
type tagField struct {
	index []int        // reflect field index (supports embedded structs)
	name  string       // param name from struct tag
	typ   reflect.Type // original field type (used by multipart binding)
	kind  reflect.Kind // for type-switch during assignment (elem kind if isPtr)
	isPtr bool         // true if field is a pointer type (*string, *int, etc.)
}

// tagCacheKey identifies a (struct type, tag name) pair for caching.
type tagCacheKey struct {
	typ reflect.Type
	tag string
}

// tagTypeCache caches parsed struct tag info per (type, tag) pair.
// Key: tagCacheKey, Value: []tagField
var tagTypeCache sync.Map

// buildTagFields walks struct fields and reads tags with the given name.
// Fields without the tag or tagged with "-" are skipped.
func buildTagFields(t reflect.Type, tagName string) []tagField {
	var fields []tagField
	buildTagFieldsRecursive(t, tagName, nil, &fields)
	return fields
}

func buildTagFieldsRecursive(t reflect.Type, tagName string, parentIndex []int, fields *[]tagField) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		idx := make([]int, len(parentIndex)+1)
		copy(idx, parentIndex)
		idx[len(parentIndex)] = i

		// Handle embedded structs recursively (including pointer-to-struct)
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				buildTagFieldsRecursive(ft, tagName, idx, fields)
				continue
			}
		}

		tag := f.Tag.Get(tagName)
		if tag == "" || tag == "-" {
			continue
		}

		// Use first comma-separated token as param name (matches json tag convention)
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}

		kind := f.Type.Kind()
		isPtr := kind == reflect.Pointer
		if isPtr {
			kind = f.Type.Elem().Kind()
		}
		// For slices, record the slice kind so we know to assign all values
		*fields = append(*fields, tagField{
			index: idx,
			name:  name,
			typ:   f.Type,
			kind:  kind,
			isPtr: isPtr,
		})
	}
}

func cachedTagFields(t reflect.Type, tagName string) []tagField {
	key := tagCacheKey{typ: t, tag: tagName}
	cached, ok := tagTypeCache.Load(key)
	if !ok {
		cached = buildTagFields(t, tagName)
		tagTypeCache.Store(key, cached)
	}
	return cached.([]tagField)
}

func structValue(target any) (reflect.Value, reflect.Type, bool) {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, nil, false
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return reflect.Value{}, nil, false
	}
	return rv, rv.Type(), true
}

// decodeValues decodes url.Values into a struct using the specified struct tag.
// Both BindQuery (tag "query") and BindBody form/multipart (tag "form") use this.
func decodeValues(target any, values url.Values, tagName string) error {
	rv, t, ok := structValue(target)
	if !ok {
		return NewHTTPError(http.StatusBadRequest, "bind target must be a non-nil pointer to struct")
	}

	tfields := cachedTagFields(t, tagName)

	for _, tf := range tfields {
		vals, exists := values[tf.name]
		if !exists || len(vals) == 0 {
			continue
		}

		field := fieldByIndexAlloc(rv, tf.index)
		if err := setTagField(field, tf, vals); err != nil {
			return err
		}
	}

	return nil
}

// setTagField assigns value(s) to a struct field, converting types as needed.
func setTagField(field reflect.Value, tf tagField, vals []string) error {
	// Handle pointer fields: allocate the pointed-to value, then set through it.
	// After this block, field points to the allocated elem so the logic below
	// works unchanged.
	if tf.isPtr {
		ptr := reflect.New(field.Type().Elem())
		field.Set(ptr)
		field = ptr.Elem()
	}

	if tf.kind == reflect.Slice {
		return setSliceField(field, tf.name, vals)
	}

	return setScalarField(field, tf.kind, tf.name, vals[0])
}

// setSliceField fills a slice field element by element, so []int, []float64,
// and slices of TextUnmarshaler-implementing elements all bind from repeated
// params. Plain []string keeps a copy-free fast path.
func setSliceField(field reflect.Value, name string, vals []string) error {
	if field.Type() == stringSliceType {
		field.Set(reflect.ValueOf(vals))
		return nil
	}

	out := reflect.MakeSlice(field.Type(), len(vals), len(vals))
	elemKind := field.Type().Elem().Kind()
	for i, val := range vals {
		if err := setScalarField(out.Index(i), elemKind, name, val); err != nil {
			return err
		}
	}
	field.Set(out)
	return nil
}

// setScalarField converts one string value into field. Types implementing
// [encoding.TextUnmarshaler] (time.Time, netip.Addr, custom types) take
// precedence over the native kind conversions, matching the config
// decoder's TextUnmarshaller hook.
func setScalarField(field reflect.Value, kind reflect.Kind, name, val string) error {
	if field.CanAddr() {
		if tu, ok := field.Addr().Interface().(encoding.TextUnmarshaler); ok {
			if err := tu.UnmarshalText([]byte(val)); err != nil {
				return NewHTTPError(http.StatusBadRequest, "invalid value for field '"+name+"'")
			}
			return nil
		}
	}

	switch kind {
	case reflect.String:
		field.SetString(val)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(val, 10, intBitSize(kind))
		if err != nil {
			return NewHTTPError(http.StatusBadRequest, "invalid integer value for field '"+name+"'")
		}
		field.SetInt(n)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(val, 10, intBitSize(kind))
		if err != nil {
			return NewHTTPError(http.StatusBadRequest, "invalid unsigned integer value for field '"+name+"'")
		}
		field.SetUint(n)

	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(val, floatBitSize(kind))
		if err != nil {
			return NewHTTPError(http.StatusBadRequest, "invalid float value for field '"+name+"'")
		}
		field.SetFloat(n)

	case reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return NewHTTPError(http.StatusBadRequest, "invalid boolean value for field '"+name+"'")
		}
		field.SetBool(b)

	default:
		return NewHTTPError(http.StatusBadRequest,
			"unsupported field type '"+kind.String()+"' for field '"+name+"'")
	}

	return nil
}

// intBitSize returns the strconv bitSize for the given integer reflect.Kind.
func intBitSize(k reflect.Kind) int {
	switch k {
	case reflect.Int8, reflect.Uint8:
		return 8
	case reflect.Int16, reflect.Uint16:
		return 16
	case reflect.Int32, reflect.Uint32:
		return 32
	default:
		return 64
	}
}

// floatBitSize returns the strconv bitSize for the given float reflect.Kind.
func floatBitSize(k reflect.Kind) int {
	if k == reflect.Float32 {
		return 32
	}
	return 64
}

// fieldByIndexAlloc walks the index path like reflect.Value.FieldByIndex,
// but auto-allocates nil pointer intermediates. This supports
// pointer-to-embedded-struct fields (e.g., *Base in struct { *Base }).
func fieldByIndexAlloc(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v
}

// bindMultipartFiles binds multipart file fields to struct fields tagged with `form:"name"`.
// Supports *multipart.FileHeader (single file) and []*multipart.FileHeader (multiple files).
func bindMultipartFiles(target any, files map[string][]*multipart.FileHeader) error {
	if len(files) == 0 {
		return nil
	}

	rv, t, ok := structValue(target)
	if !ok {
		return nil
	}
	for _, tf := range cachedTagFields(t, "form") {
		fhs, exists := files[tf.name]
		if !exists || len(fhs) == 0 {
			continue
		}

		field := fieldByIndexAlloc(rv, tf.index)

		// *multipart.FileHeader — single file
		if tf.typ == multipartFileHeaderPtrType {
			field.Set(reflect.ValueOf(fhs[0]))
			continue
		}

		// []*multipart.FileHeader — multiple files
		if tf.typ.Kind() == reflect.Slice && tf.typ.Elem() == multipartFileHeaderPtrType {
			field.Set(reflect.ValueOf(fhs))
		}
	}
	return nil
}
