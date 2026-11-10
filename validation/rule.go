// Copyright (c) 2016 Qiang Xue.
// Originally derived from github.com/go-ozzo/ozzo-validation (MIT License).
//
// The findStructField algorithm, getErrorFieldName logic, and ValidateStruct
// orchestration are adapted from ozzo-validation's struct.go. Credo diverges
// with generic Rule[T], offset-based caching, and []Violation errors.

package validation

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Rule is the generic validation rule interface. The type parameter T
// matches the field type, providing compile-time type safety.
type Rule[T any] interface {
	Validate(value T) error
}

// funcRule adapts a plain function to [Rule]. Built-in rules whose only state
// is their constructor arguments are closures over funcRule; composite rules
// that hold other rules (Each, When, NilSafe) keep explicit types.
type funcRule[T any] func(value T) error

// Validate implements [Rule].
func (f funcRule[T]) Validate(value T) error { return f(value) }

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
// Once the rules pass, a field whose value implements Validatable is
// validated by its Validate (nested struct support), and its failures are
// prefixed with the field's name; a field whose rules fail reports their
// violations only. A slice or array whose elements implement Validatable, or
// are structs whose pointer does, has each element validated the same way,
// its failures prefixed with the field's name and the element's index, as in
// items[0].name. A struct field's or element's Validate runs on the value
// itself, so what a pointer-receiver Validate normalizes is kept. A nil
// pointer or nil interface is valid and its Validate is not called; presence
// is what [NotNil] and [Required] check, so
//
//	validation.Field(&r.Billing, validation.NotNil[Address]())
//
// requires the address and then validates it.
func Field[T any](fieldPtr *T, rules ...Rule[T]) FieldRules {
	return &fieldRules[T]{
		ptr:   fieldPtr,
		rules: rules,
	}
}

// ValidateStruct validates a struct by running all field rules. Returns nil
// if all validations pass, or [Violations] containing all failures. A rule error
// that is neither a *Violation nor [Violations] is internal: validation
// stops at once and ValidateStruct returns that error unchanged.
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

	var allErrors Violations
	for _, f := range fields {
		if err := f.validate(structPtr); err != nil {
			if internal := collectErrors(&allErrors, err, ""); internal != nil {
				return internal
			}
		}
	}

	if len(allErrors) == 0 {
		return nil
	}
	return allErrors
}

func (f *fieldRules[T]) validate(structPtr any) error {
	fieldName := resolveFieldName(structPtr, f.ptr)

	if len(f.rules) > 0 {
		value := *f.ptr
		var fieldErrors Violations
		for _, rule := range f.rules {
			if err := rule.Validate(value); err != nil {
				if internal := collectErrors(&fieldErrors, err, fieldName); internal != nil {
					return internal
				}
			}
		}
		// A field whose rules fail reports their violations only, so an Each
		// rule and the element validation below never report one violation
		// twice.
		if len(fieldErrors) > 0 {
			return fieldErrors
		}
	}

	// Once the rules pass, a value that implements Validatable, or a slice or
	// array of such values, is validated by its own Validate.
	return validateNested(fieldName, f.ptr)
}

// Validatable is implemented by types that can validate themselves.
// When a target struct implements Validatable, the Bind methods
// (BindBody, BindQuery) automatically call Validate()
// after decoding ("parse, don't validate").
//
// Nested fields that implement Validatable, and the elements of a slice or
// array of them, are validated by [Field] once the field's rules pass; a nil
// nested pointer is valid.
type Validatable interface {
	Validate() error
}

// nestedMode is how a value of one type is validated after its field's
// rules. It depends only on the type, so it is computed once per type.
type nestedMode uint8

const (
	// nestedNone: the type has no Validate to run.
	nestedNone nestedMode = iota
	// nestedAddr: a struct whose pointer implements Validatable, validated
	// through its own address so a pointer-receiver Validate keeps what it
	// normalizes.
	nestedAddr
	// nestedValue: a pointer, an interface or another value that implements,
	// or may hold a value that implements, Validatable.
	nestedValue
	// nestedElems: a slice or array whose elements are validated.
	nestedElems
)

// nestedPlan is the cached nestedMode of a field type and, for nestedElems,
// of its element type.
type nestedPlan struct {
	mode nestedMode
	elem nestedMode
}

var validatableType = reflect.TypeFor[Validatable]()

// nestedPlans caches a nestedPlan per field type.
// Key: reflect.Type, Value: nestedPlan
var nestedPlans sync.Map

func nestedPlanFor(t reflect.Type) nestedPlan {
	// A basic type without methods, the common field, never nests: skip the
	// cache lookup.
	if k := t.Kind(); k >= reflect.Bool && k <= reflect.Complex128 || k == reflect.String {
		if t.NumMethod() == 0 {
			return nestedPlan{}
		}
	}
	if cached, ok := nestedPlans.Load(t); ok {
		return cached.(nestedPlan)
	}
	p := nestedPlan{mode: valueNestedMode(t)}
	if p.mode == nestedNone && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		if em := valueNestedMode(t.Elem()); em != nestedNone {
			p = nestedPlan{mode: nestedElems, elem: em}
		}
	}
	nestedPlans.Store(t, p)
	return p
}

// valueNestedMode is how a single value of type t is validated. A type with a
// Validate of its own, a slice type included, is validated by it rather than
// element by element.
func valueNestedMode(t reflect.Type) nestedMode {
	switch {
	case t.Kind() == reflect.Struct:
		if reflect.PointerTo(t).Implements(validatableType) {
			return nestedAddr
		}
		return nestedNone
	case t.Kind() == reflect.Interface, t.Implements(validatableType):
		return nestedValue
	}
	return nestedNone
}

// validateNested runs the Validate of the field fieldPtr points to, or of its
// elements, when it has one. Failures are prefixed with the field name, and an
// element's with the field name and its index; an internal error is returned
// unchanged.
//
// A struct field, and a struct element, is validated through its own pointer,
// so a pointer-receiver Validate keeps what it normalizes and nothing is
// copied. A nil pointer or nil interface is valid and its Validate is not
// called: presence is what [NotNil] and [Required] check.
func validateNested[T any](fieldName string, fieldPtr *T) error {
	// The common field types never nest; a type switch decides them without
	// reflection.
	switch any(fieldPtr).(type) {
	case *string, *bool, *int, *int64, *int32, *uint, *uint64, *float64, *float32:
		return nil
	}
	p := nestedPlanFor(reflect.TypeFor[T]())
	switch p.mode {
	case nestedAddr:
		return runValidate(fieldName, any(fieldPtr).(Validatable))
	case nestedValue:
		if v := validatableValue(any(*fieldPtr)); v != nil {
			return runValidate(fieldName, v)
		}
	case nestedElems:
		return validateElements(fieldName, reflect.ValueOf(fieldPtr).Elem(), p.elem)
	}
	return nil
}

// validateElements validates each element of the slice or array rv, an
// addressable value, by the element mode mode.
func validateElements(fieldName string, rv reflect.Value, mode nestedMode) error {
	var allErrors Violations
	for i := range rv.Len() {
		ev := rv.Index(i)
		var v Validatable
		if mode == nestedAddr {
			v = ev.Addr().Interface().(Validatable)
		} else {
			v = validatableValue(ev.Interface())
		}
		if v == nil {
			continue
		}
		if err := v.Validate(); err != nil {
			prefix := joinFieldPath(fieldName, "["+strconv.Itoa(i)+"]")
			if internal := collectErrors(&allErrors, err, prefix); internal != nil {
				return internal
			}
		}
	}
	if len(allErrors) == 0 {
		return nil
	}
	return allErrors
}

// validatableValue returns value as a Validatable, or nil when it is not one
// or is a nil pointer, whose Validate is not called.
func validatableValue(value any) Validatable {
	v, ok := value.(Validatable)
	if !ok {
		return nil
	}
	if rv := reflect.ValueOf(value); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	return v
}

// runValidate calls v's Validate and prefixes its failures with fieldName.
func runValidate(fieldName string, v Validatable) error {
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

	// pointers holds the field index of each embedded pointer to a struct
	// whose fields are promoted. Those fields live in the pointee, not in the
	// struct, so they are named by their offset in the pointee.
	pointers [][]int
}

// fieldNameCache caches *fieldMappings per struct type.
// Key: reflect.Type, Value: *fieldMappings
var fieldNameCache sync.Map

// resolveFieldName returns the name of the field fieldPtr points to in the
// struct structPtr points to, or "" when fieldPtr is no field of it.
func resolveFieldName(structPtr any, fieldPtr any) string {
	fieldVal := reflect.ValueOf(fieldPtr)
	return lookupFieldName(reflect.ValueOf(structPtr).Elem(), fieldVal.Pointer(), fieldVal.Type().Elem(), nil)
}

// lookupFieldName names the field at addr of type ft in the addressable
// struct v. A field inside v is found by its offset, preferring a precise
// offset+type match over an offset-only one; a field promoted through an
// embedded pointer is looked up in the pointee. visited holds the structs
// already searched, so a pointer cycle ends.
func lookupFieldName(v reflect.Value, addr uintptr, ft reflect.Type, visited []uintptr) string {
	t := v.Type()
	m := getFieldMappings(t)
	base := v.UnsafeAddr()
	if addr >= base && addr-base < max(t.Size(), 1) {
		offset := addr - base
		if name, ok := m.byOffsetType[offsetTypeKey{offset: offset, typ: ft}]; ok {
			return name
		}
		// Fallback: offset-only match (covers most cases).
		if name, ok := m.byOffset[offset]; ok {
			return name
		}
	}
	if len(m.pointers) == 0 {
		return ""
	}
	visited = append(visited, base)
	for _, index := range m.pointers {
		p := v.FieldByIndex(index)
		if p.IsNil() || slices.Contains(visited, p.Pointer()) {
			continue
		}
		if name := lookupFieldName(p.Elem(), addr, ft, visited); name != "" {
			return name
		}
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
	list, pointers := buildFieldMappingList(t, 0, nil)
	m := &fieldMappings{
		byOffsetType: make(map[offsetTypeKey]string, len(list)),
		byOffset:     make(map[uintptr]string, len(list)),
		pointers:     pointers,
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
// mappings in declaration order, as encoding/json sees the fields: an
// exported field is named, and an embedded struct promotes its exported
// fields even when its type is unexported. An embedded pointer to an exported
// struct type is named and its index returned among the pointers, whose
// pointee's fields are mapped by the pointee's own offsets; an embedded
// pointer to an unexported struct type is skipped, as no binder can allocate
// it. baseOffset is added to each field's offset for embedded structs, and
// index is the field index of t in the struct the walk started from.
func buildFieldMappingList(t reflect.Type, baseOffset uintptr, index []int) (mappings []fieldMapping, pointers [][]int) {
	for i := range t.NumField() {
		f := t.Field(i)
		offset := baseOffset + f.Offset
		fieldIndex := append(slices.Clone(index), i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if f.IsExported() {
				mappings = append(mappings, fieldMapping{offset: offset, typ: f.Type, name: getErrorFieldName(f)})
			}
			inner, innerPointers := buildFieldMappingList(f.Type, offset, fieldIndex)
			mappings = append(mappings, inner...)
			pointers = append(pointers, innerPointers...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		mappings = append(mappings, fieldMapping{offset: offset, typ: f.Type, name: getErrorFieldName(f)})
		if f.Anonymous && f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct {
			pointers = append(pointers, fieldIndex)
		}
	}
	return mappings, pointers
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
