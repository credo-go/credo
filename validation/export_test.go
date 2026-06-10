package validation

import (
	"reflect"
	"sync"
)

// ExportResolveFieldName exposes resolveFieldName for testing.
func ExportResolveFieldName(structPtr any, fieldPtr any) string {
	return resolveFieldName(structPtr, fieldPtr)
}

// ExportBuildFieldMappings exposes buildFieldMappingList for testing.
func ExportBuildFieldMappings(t reflect.Type) []FieldMappingExport {
	mappings := buildFieldMappingList(t, 0)
	result := make([]FieldMappingExport, len(mappings))
	for i, m := range mappings {
		result[i] = FieldMappingExport{
			Offset: m.offset,
			Typ:    m.typ,
			Name:   m.name,
		}
	}
	return result
}

// FieldMappingExport is an exported version of fieldMapping for tests.
type FieldMappingExport struct {
	Offset uintptr
	Typ    reflect.Type
	Name   string
}

// ExportGetErrorFieldName exposes getErrorFieldName for testing.
func ExportGetErrorFieldName(f reflect.StructField) string {
	return getErrorFieldName(f)
}

// ExportNewRuleError exposes newRuleError for testing.
func ExportNewRuleError(code, message string, params map[string]any) *ValidationError {
	return newRuleError(code, message, params)
}

// ExportToValidationError exposes toValidationError for testing.
func ExportToValidationError(err error) *ValidationError {
	return toValidationError(err)
}

// ExportJoinFieldPath exposes joinFieldPath for testing.
func ExportJoinFieldPath(parent, child string) string {
	return joinFieldPath(parent, child)
}

// ExportPrefixErrors exposes prefixErrors for testing.
func ExportPrefixErrors(prefix string, err error) Errors {
	return prefixErrors(prefix, err)
}

// ResetFieldNameCache clears the field name cache for test isolation.
func ResetFieldNameCache() {
	fieldNameCache = sync.Map{}
}
