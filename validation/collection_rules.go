package validation

import (
	"strconv"
)

// NotEmptySlice creates a [Rule] that fails when the slice has no elements
// (nil or zero length). It is the slice counterpart of [Required], whose
// comparable constraint excludes slices; the check is len-based — no
// reflection.
//
//	validation.Field(&o.Items, validation.NotEmptySlice[Item]())
func NotEmptySlice[E any]() Rule[[]E] {
	return &notEmptySliceRule[E]{}
}

type notEmptySliceRule[E any] struct{}

func (r *notEmptySliceRule[E]) Validate(value []E) error {
	if len(value) == 0 {
		return newRuleError("not_empty", "must not be empty", nil)
	}
	return nil
}

// NotEmptyMap creates a [Rule] that fails when the map has no entries
// (nil or zero length). It is the map counterpart of [Required], whose
// comparable constraint excludes maps; the check is len-based — no
// reflection.
//
//	validation.Field(&c.Limits, validation.NotEmptyMap[string, int]())
func NotEmptyMap[K comparable, V any]() Rule[map[K]V] {
	return &notEmptyMapRule[K, V]{}
}

type notEmptyMapRule[K comparable, V any] struct{}

func (r *notEmptyMapRule[K, V]) Validate(value map[K]V) error {
	if len(value) == 0 {
		return newRuleError("not_empty", "must not be empty", nil)
	}
	return nil
}

// Each creates a [Rule] that validates each element of a slice against the
// given rules. Error fields use bracket notation: "[0]", "[1]", etc.
func Each[T any](rules ...Rule[T]) Rule[[]T] {
	return &eachRule[T]{rules: rules}
}

type eachRule[T any] struct {
	rules []Rule[T]
}

func (r *eachRule[T]) Validate(value []T) error {
	var allErrors Errors
	for i, elem := range value {
		for _, rule := range r.rules {
			if err := rule.Validate(elem); err != nil {
				prefix := "[" + strconv.Itoa(i) + "]"
				collectErrors(&allErrors, err, prefix)
			}
		}
	}
	if len(allErrors) == 0 {
		return nil
	}
	return allErrors
}

// When creates a [Rule] that conditionally applies rules based on a boolean
// condition. When the condition is false, validation is skipped (returns nil).
// When true, all inner rules are executed and errors are collected.
//
//	validation.Field(&o.CardNumber,
//	    validation.When(o.PaymentMethod == "card",
//	        validation.Required[string](), validation.Length(13, 19),
//	    ),
//	)
func When[T any](condition bool, rules ...Rule[T]) Rule[T] {
	return &whenRule[T]{condition: condition, rules: rules}
}

type whenRule[T any] struct {
	condition bool
	rules     []Rule[T]
}

func (r *whenRule[T]) Validate(value T) error {
	if !r.condition {
		return nil
	}
	var allErrors Errors
	for _, rule := range r.rules {
		if err := rule.Validate(value); err != nil {
			collectErrors(&allErrors, err, "")
		}
	}
	if len(allErrors) == 0 {
		return nil
	}
	return allErrors
}

// NilSafe wraps rules for use with pointer fields. When the pointer is nil,
// validation is skipped. When non-nil, the value is dereferenced and inner
// rules execute. Used for PATCH/partial update support.
//
//	type UpdateUserInput struct {
//	    Name *string `json:"name"`
//	}
//
//	validation.Field(&u.Name, validation.NilSafe(validation.Length(2, 100)))
func NilSafe[T any](rules ...Rule[T]) Rule[*T] {
	return &nilSafeRule[T]{rules: rules}
}

type nilSafeRule[T any] struct {
	rules []Rule[T]
}

func (r *nilSafeRule[T]) Validate(value *T) error {
	if value == nil {
		return nil
	}
	var allErrors Errors
	for _, rule := range r.rules {
		if err := rule.Validate(*value); err != nil {
			collectErrors(&allErrors, err, "")
		}
	}
	if len(allErrors) == 0 {
		return nil
	}
	return allErrors
}
