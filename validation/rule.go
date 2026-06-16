// Copyright (c) 2016 Qiang Xue.
// Originally derived from github.com/go-ozzo/ozzo-validation (MIT License).
//
// The findStructField algorithm, getErrorFieldName logic, and ValidateStruct
// orchestration are adapted from ozzo-validation's struct.go. Credo diverges
// with generic Rule[T], offset-based caching, and []ValidationError errors.

package validation

import (
	"reflect"
	"strings"
	"sync"
)

// Rule is the generic validation rule interface. The type parameter T
// matches the field type, providing compile-time type safety.
type Rule[T any] interface {
	Validate(value T) error
}

// FieldRules is a type-erased container returned by [Field]. The unexported
// methods prevent external implementation — only [Field] can produce values
// satisfying this interface.
type FieldRules interface {
	validate(structPtr any) error
}

// fieldRules is the concrete generic implementation of [FieldRules].
type fieldRules[T any] struct {
	ptr   *T
	rules []Rule[T]
}

// Field creates a [FieldRules] that validates the struct field pointed to by
// fieldPtr using the given rules. The type parameter T is inferred from
// fieldPtr.
//
// When fieldPtr points to a field that implements Validatable and no explicit
// rules are given, Validate() is called automatically (nested struct support).
func Field[T any](fieldPtr *T, rules ...Rule[T]) FieldRules {
	return &fieldRules[T]{
		ptr:   fieldPtr,
		rules: rules,
	}
}

// ValidateStruct validates a struct by running all field rules. Returns nil
// if all validations pass, or [Errors] containing all failures.
//
// structPtr must be a non-nil pointer to a struct. Passing a nil pointer
// returns nil (valid). Passing a non-pointer or pointer to non-struct panics.
func ValidateStruct(structPtr any, fields ...FieldRules) error {
	v := reflect.ValueOf(structPtr)
	if v.Kind() != reflect.Pointer {
		panic("validation: ValidateStruct requires a pointer to a struct")
	}
	if v.IsNil() {
		return nil
	}
	if v.Elem().Kind() != reflect.Struct {
		panic("validation: ValidateStruct requires a pointer to a struct")
	}

	var allErrors Errors
	for _, f := range fields {
		if err := f.validate(structPtr); err != nil {
			collectErrors(&allErrors, err, "")
		}
	}

	if len(allErrors) == 0 {
		return nil
	}
	return allErrors
}

func (f *fieldRules[T]) validate(structPtr any) error {
	fieldName := resolveFieldName(structPtr, f.ptr)
	value := *f.ptr

	// If no explicit rules, check if the value implements Validatable
	// (nested struct auto-validation).
	if len(f.rules) == 0 {
		return validateNested(fieldName, value)
	}

	var fieldErrors Errors
	for _, rule := range f.rules {
		if err := rule.Validate(value); err != nil {
			collectErrors(&fieldErrors, err, fieldName)
		}
	}

	if len(fieldErrors) == 0 {
		return nil
	}
	return fieldErrors
}

// Validatable is implemented by types that can validate themselves.
// When a target struct implements Validatable, the Bind methods
// (BindBody, BindQuery) automatically call Validate()
// after decoding ("parse, don't validate").
//
// Nested struct fields that implement Validatable are auto-validated
// by [Field] when no explicit rules are given.
type Validatable interface {
	Validate() error
}

// validateNested checks if value implements Validatable and calls Validate().
// Errors are prefixed with the field name. Handles both value and pointer
// receivers: if the value itself doesn't implement Validatable, a pointer
// to the value is tried.
func validateNested(fieldName string, value any) error {
	v, ok := value.(Validatable)
	if !ok {
		// Try pointer receiver: if value is addressable via reflect,
		// create a pointer and check again.
		rv := reflect.ValueOf(value)
		if rv.Kind() == reflect.Struct {
			ptr := reflect.New(rv.Type())
			ptr.Elem().Set(rv)
			v, ok = ptr.Interface().(Validatable)
		}
		if !ok {
			return nil
		}
	}
	err := v.Validate()
	if err == nil {
		return nil
	}
	return prefixErrors(fieldName, err)
}

// --- Field name resolution with caching ---

// fieldMapping maps a struct field's offset and type to its JSON/Go name.
type fieldMapping struct {
	offset uintptr
	typ    reflect.Type
	name   string
}

// offsetTypeKey identifies a field by its offset and concrete type. It is used
// for the precise lookup that disambiguates fields sharing an offset (e.g. an
// embedded struct base and its first field).
type offsetTypeKey struct {
	offset uintptr
	typ    reflect.Type
}

// fieldMappings holds precomputed lookups for a struct type so that field-name
// resolution is O(1) per field instead of a linear scan on every validation.
type fieldMappings struct {
	byOffsetType map[offsetTypeKey]string // precise offset+type match
	byOffset     map[uintptr]string       // offset-only fallback
}

// fieldNameCache caches *fieldMappings per struct type.
// Key: reflect.Type, Value: *fieldMappings
var fieldNameCache sync.Map

// resolveFieldName computes the offset of fieldPtr relative to structPtr and
// returns the matching field name from the cached lookups. It prefers a precise
// offset+type match and falls back to an offset-only match.
func resolveFieldName(structPtr any, fieldPtr any) string {
	structVal := reflect.ValueOf(structPtr)
	structPtrAddr := structVal.Pointer()
	fieldPtrAddr := reflect.ValueOf(fieldPtr).Pointer()
	offset := fieldPtrAddr - structPtrAddr

	structType := structVal.Elem().Type()
	m := getFieldMappings(structType)

	fieldType := reflect.TypeOf(fieldPtr).Elem()
	if name, ok := m.byOffsetType[offsetTypeKey{offset: offset, typ: fieldType}]; ok {
		return name
	}
	// Fallback: offset-only match (covers most cases).
	if name, ok := m.byOffset[offset]; ok {
		return name
	}

	return ""
}

// getFieldMappings returns cached field mappings for the given struct type,
// building them on first access.
func getFieldMappings(t reflect.Type) *fieldMappings {
	if cached, ok := fieldNameCache.Load(t); ok {
		return cached.(*fieldMappings)
	}
	m := buildFieldMappings(t)
	fieldNameCache.Store(t, m)
	return m
}

// buildFieldMappings walks a struct type and folds its fields into the
// offset-based lookups. When multiple fields share an offset (an embedded
// struct base vs. its first field), the first encountered in declaration order
// wins — matching the previous linear-scan behaviour.
func buildFieldMappings(t reflect.Type) *fieldMappings {
	list := buildFieldMappingList(t, 0)
	m := &fieldMappings{
		byOffsetType: make(map[offsetTypeKey]string, len(list)),
		byOffset:     make(map[uintptr]string, len(list)),
	}
	for _, fm := range list {
		key := offsetTypeKey{offset: fm.offset, typ: fm.typ}
		if _, exists := m.byOffsetType[key]; !exists {
			m.byOffsetType[key] = fm.name
		}
		if _, exists := m.byOffset[fm.offset]; !exists {
			m.byOffset[fm.offset] = fm.name
		}
	}
	return m
}

// buildFieldMappingList walks a struct type's fields and builds offset-to-name
// mappings in declaration order. baseOffset is added to each field's offset for
// embedded structs.
func buildFieldMappingList(t reflect.Type, baseOffset uintptr) []fieldMapping {
	var mappings []fieldMapping
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		offset := baseOffset + f.Offset
		name := getErrorFieldName(f)
		mappings = append(mappings, fieldMapping{
			offset: offset,
			typ:    f.Type,
			name:   name,
		})

		// Recurse into embedded (anonymous) struct fields.
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				mappings = append(mappings, buildFieldMappingList(ft, offset)...)
			}
		}
	}
	return mappings
}

// getErrorFieldName returns the field name for error reporting.
// Reads the json struct tag first; falls back to the Go field name.
// Adapted from ozzo-validation.
func getErrorFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag != "" && tag != "-" {
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			return name
		}
	}
	return f.Name
}
