package validation_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/credo-go/credo/validation"
)

// lineItem has a pointer-receiver Validate that counts its calls.
type lineItem struct {
	Name  string `json:"name"`
	calls *int
}

func (li *lineItem) Validate() error {
	if li.calls != nil {
		*li.calls++
	}
	if li.Name == "" {
		return validation.Errors{{Field: "name", Code: "required", Message: "is required"}}
	}
	return nil
}

// lineItems is a slice type with a Validate of its own.
type lineItems []lineItem

func (l lineItems) Validate() error {
	if len(l) > 1 {
		return validation.NewError("too_many", "at most one item")
	}
	return nil
}

func violationFields(t *testing.T, err error) []string {
	t.Helper()
	errs, ok := errors.AsType[validation.Errors](err)
	if !ok {
		t.Fatalf("err = %v (%T), want validation.Errors", err, err)
	}
	fields := make([]string, len(errs))
	for i, e := range errs {
		fields[i] = e.Field
	}
	return fields
}

func TestValidateStruct_SliceOfStructsIsValidated(t *testing.T) {
	type Input struct {
		Items    []lineItem               `json:"items"`
		Ptrs     []*lineItem              `json:"ptrs"`
		Values   []valueAddress           `json:"values"`
		Array    [2]trimmingAddress       `json:"array"`
		Ifaces   []validation.Validatable `json:"ifaces"`
		Optional []lineItem               `json:"optional"`
	}
	input := &Input{
		Items:  []lineItem{{Name: "ok"}, {}, {Name: "ok"}, {}},
		Ptrs:   []*lineItem{nil, {}},
		Values: []valueAddress{{City: "Ankara"}, {}},
		Array:  [2]trimmingAddress{{City: " a "}, {City: " b "}},
		Ifaces: []validation.Validatable{nil, (*lineItem)(nil), &lineItem{}},
	}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Items),
		validation.Field(&input.Ptrs),
		validation.Field(&input.Values),
		validation.Field(&input.Array),
		validation.Field(&input.Ifaces),
		validation.Field(&input.Optional),
	)

	want := []string{"items[1].name", "items[3].name", "ptrs[1].name", "values[1].city", "ifaces[2].name"}
	if got := violationFields(t, err); !slices.Equal(got, want) {
		t.Fatalf("fields = %q, want %q", got, want)
	}
	if input.Array[0].City != "a" || input.Array[1].City != "b" {
		t.Fatalf("Array = %+v, want each element's Validate to normalize the element itself", input.Array)
	}
}

func TestValidateStruct_SliceElementsAfterTheFieldsRules(t *testing.T) {
	type Input struct {
		Items []lineItem `json:"items"`
	}

	t.Run("an Each rule that passes still runs the elements' Validate", func(t *testing.T) {
		calls := 0
		input := &Input{Items: []lineItem{{Name: "ok", calls: &calls}, {calls: &calls}}}
		err := validation.ValidateStruct(input,
			validation.Field(&input.Items,
				validation.NotEmptySlice[lineItem](),
				validation.Each[lineItem](alwaysPass[lineItem]{}),
			),
		)
		if got, want := violationFields(t, err), []string{"items[1].name"}; !slices.Equal(got, want) {
			t.Fatalf("fields = %q, want %q", got, want)
		}
		if calls != 2 {
			t.Fatalf("Validate calls = %d, want 2", calls)
		}
	})

	t.Run("an Each rule that fails reports its violations once", func(t *testing.T) {
		calls := 0
		input := &Input{Items: []lineItem{{calls: &calls}, {calls: &calls}}}
		err := validation.ValidateStruct(input,
			validation.Field(&input.Items,
				validation.Each[lineItem](alwaysFail[lineItem]{code: "each", message: "element failed"}),
			),
		)
		errs, ok := errors.AsType[validation.Errors](err)
		if !ok || len(errs) != 2 {
			t.Fatalf("err = %v, want the two Each violations only", err)
		}
		for i, e := range errs {
			if e.Code != "each" || e.Field != []string{"items[0]", "items[1]"}[i] {
				t.Fatalf("violation %d = %+v, want Each's violation on items[%d]", i, e, i)
			}
		}
		if calls != 0 {
			t.Fatalf("Validate calls = %d, want 0: a field whose rules fail reports their violations only", calls)
		}
	})

	t.Run("an empty slice that fails its rule is not walked", func(t *testing.T) {
		input := &Input{}
		err := validation.ValidateStruct(input,
			validation.Field(&input.Items, validation.NotEmptySlice[lineItem]()),
		)
		if got, want := violationFields(t, err), []string{"items"}; !slices.Equal(got, want) {
			t.Fatalf("fields = %q, want %q", got, want)
		}
	})
}

func TestValidateStruct_SliceTypeWithItsOwnValidate(t *testing.T) {
	type Input struct {
		Items lineItems `json:"items"`
	}
	calls := 0
	input := &Input{Items: lineItems{{calls: &calls}, {calls: &calls}}}

	err := validation.ValidateStruct(input, validation.Field(&input.Items))

	errs, ok := errors.AsType[validation.Errors](err)
	if !ok || len(errs) != 1 || errs[0].Field != "items" || errs[0].Code != "too_many" {
		t.Fatalf("err = %v, want the slice type's own violation on items", err)
	}
	if calls != 0 {
		t.Fatalf("element Validate calls = %d, want 0: a slice type's own Validate replaces the element walk", calls)
	}
}

// failingItem's Validate fails with an internal error.
type failingItem struct{}

var errItemLookup = errors.New("lookup unavailable")

func (failingItem) Validate() error { return errItemLookup }

func TestValidateStruct_SliceElementInternalErrorStops(t *testing.T) {
	type Input struct {
		Items []failingItem `json:"items"`
		Name  string        `json:"name"`
	}
	input := &Input{Items: []failingItem{{}}}

	err := validation.ValidateStruct(input,
		validation.Field(&input.Items),
		validation.Field(&input.Name, alwaysFail[string]{code: "required", message: "is required"}),
	)
	if !errors.Is(err, errItemLookup) {
		t.Fatalf("err = %v, want the element's internal error unchanged", err)
	}
	if _, ok := errors.AsType[validation.Errors](err); ok {
		t.Fatalf("err = %v, an internal error must not become a violation", err)
	}
}

// BenchmarkValidateStruct_SliceOfStructs measures the element walk over ten
// valid pointer-receiver elements.
func BenchmarkValidateStruct_SliceOfStructs(b *testing.B) {
	type Input struct {
		Items []lineItem `json:"items"`
	}
	input := &Input{Items: make([]lineItem, 10)}
	for i := range input.Items {
		input.Items[i].Name = "item"
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = validation.ValidateStruct(input, validation.Field(&input.Items))
	}
}
