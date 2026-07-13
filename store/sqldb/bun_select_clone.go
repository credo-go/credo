package sqldb

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"unsafe"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
)

// Bun v1.2.18 SelectQuery.Clone copies its documented builder/model structure,
// but omits a handful of private fields that affect execution correctness.
// The omissions can turn WherePK into an unfiltered query, discard builder
// errors and soft-delete policy, or route an explicit connection to the
// default pool.
//
// Keep Bun's Clone behavior and restore only those omitted fields. Offsets are
// discovered and type-checked once from Bun's runtime type. Open returns a
// compatibility error before allocating a pool when the private layout changes,
// rather than panicking at package initialization or risking memory corruption.
// The hot path then uses typed assignments, including Go's normal write
// barriers for interface and slice fields.
//
// Remove this compatibility layer once the pinned Bun release copies these
// fields itself. Contract tests in query_select_state_test.go guard the update.
type bunSelectCloneLayout struct {
	conn                uintptr
	err                 uintptr
	model               uintptr
	tableModel          uintptr
	flags               uintptr
	with                uintptr
	withMaterialized    uintptr
	withNotMaterialized uintptr
	whereFields         uintptr
	order               uintptr
	limit               uintptr
	offset              uintptr
	selFor              uintptr
	group               uintptr
	having              uintptr
	compound            uintptr
}

var loadBunSelectCloneLayout = sync.OnceValues(buildBunSelectCloneLayout)

func validateBunSelectCloneLayout() error {
	_, err := loadBunSelectCloneLayout()
	return err
}

func buildBunSelectCloneLayout() (bunSelectCloneLayout, error) {
	selectType := reflect.TypeFor[bun.SelectQuery]()

	conn, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[bun.IConn](),
		"whereBaseQuery", "baseQuery", "conn",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	errorField, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[error](),
		"whereBaseQuery", "baseQuery", "err",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	model, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[bun.Model](),
		"whereBaseQuery", "baseQuery", "model",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	tableModel, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[bun.TableModel](),
		"whereBaseQuery", "baseQuery", "tableModel",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	with, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[[]bun.WithQuery](),
		"whereBaseQuery", "baseQuery", "with",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	withType := reflect.TypeFor[bun.WithQuery]()
	withMaterialized, err := requireBunSelectField(
		withType,
		reflect.TypeFor[bool](),
		"materialized",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	withNotMaterialized, err := requireBunSelectField(
		withType,
		reflect.TypeFor[bool](),
		"notMaterialized",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	whereFields, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[[]*schema.Field](),
		"whereBaseQuery", "whereFields",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	order, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[[]schema.QueryWithArgs](),
		"orderLimitOffsetQuery", "order",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	limit, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[int32](),
		"orderLimitOffsetQuery", "limit",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	offset, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[int32](),
		"orderLimitOffsetQuery", "offset",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	selFor, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[schema.QueryWithArgs](),
		"selFor",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	group, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[[]schema.QueryWithArgs](),
		"group",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	having, err := requireBunSelectField(
		selectType,
		reflect.TypeFor[[]schema.QueryWithArgs](),
		"having",
	)
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	compound, compoundType, err := bunSelectField(selectType, "union")
	if err != nil {
		return bunSelectCloneLayout{}, err
	}
	if compoundType.Kind() != reflect.Slice || compoundType.Size() != unsafe.Sizeof([]byte(nil)) {
		return bunSelectCloneLayout{}, fmt.Errorf(
			"sqldb: incompatible Bun SelectQuery.union layout: got %s (%s, %d bytes)",
			compoundType,
			compoundType.Kind(),
			compoundType.Size(),
		)
	}

	flagsOffset, flagsType, fieldErr := bunSelectField(
		selectType,
		"whereBaseQuery", "baseQuery", "flags",
	)
	if fieldErr != nil {
		return bunSelectCloneLayout{}, fieldErr
	}
	if flagsType.Kind() != reflect.Uint64 || flagsType.Size() != unsafe.Sizeof(uint64(0)) {
		return bunSelectCloneLayout{}, fmt.Errorf(
			"sqldb: incompatible Bun SelectQuery.flags layout: got %s (%s, %d bytes)",
			flagsType,
			flagsType.Kind(),
			flagsType.Size(),
		)
	}

	return bunSelectCloneLayout{
		conn:                conn,
		err:                 errorField,
		model:               model,
		tableModel:          tableModel,
		flags:               flagsOffset,
		with:                with,
		withMaterialized:    withMaterialized,
		withNotMaterialized: withNotMaterialized,
		whereFields:         whereFields,
		order:               order,
		limit:               limit,
		offset:              offset,
		selFor:              selFor,
		group:               group,
		having:              having,
		compound:            compound,
	}, nil
}

func requireBunSelectField(root, want reflect.Type, path ...string) (uintptr, error) {
	offset, got, err := bunSelectField(root, path...)
	if err != nil {
		return 0, err
	}
	if got != want {
		return 0, fmt.Errorf(
			"sqldb: incompatible Bun SelectQuery.%s layout: got %s, want %s",
			path[len(path)-1],
			got,
			want,
		)
	}
	return offset, nil
}

func bunSelectField(root reflect.Type, path ...string) (uintptr, reflect.Type, error) {
	current := root
	var offset uintptr
	for _, name := range path {
		if current.Kind() != reflect.Struct {
			return 0, nil, fmt.Errorf(
				"sqldb: incompatible Bun SelectQuery layout: %s is %s before field %q",
				current,
				current.Kind(),
				name,
			)
		}
		field, ok := current.FieldByName(name)
		if !ok {
			return 0, nil, fmt.Errorf(
				"sqldb: incompatible Bun SelectQuery layout: %s has no field %q",
				current,
				name,
			)
		}
		offset += field.Offset
		current = field.Type
	}
	return offset, current, nil
}

func cloneBunSelectQuery(raw *bun.SelectQuery) *bun.SelectQuery {
	if raw == nil {
		return nil
	}

	cloned := raw.Clone()
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return cloned.Err(err)
	}
	src := unsafe.Pointer(raw)
	dst := unsafe.Pointer(cloned)

	copyBunSelectField[bun.IConn](dst, src, layout.conn)
	copyBunSelectField[error](dst, src, layout.err)
	copyBunSelectField[uint64](dst, src, layout.flags)
	patchBunSelectWithQueries(dst, src, layout)
	cloneBunSelectSlice[*schema.Field](dst, src, layout.whereFields)

	return cloned
}

func bunSelectQueryConn(raw *bun.SelectQuery) (bun.IConn, error) {
	if raw == nil {
		return nil, nil
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return nil, err
	}
	return *(*bun.IConn)(unsafe.Add(unsafe.Pointer(raw), layout.conn)), nil
}

func bunSelectQueryError(raw *bun.SelectQuery) error {
	if raw == nil {
		return nil
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return err
	}
	return *(*error)(unsafe.Add(unsafe.Pointer(raw), layout.err))
}

func bunSelectQueryTableModel(raw *bun.SelectQuery) (bun.TableModel, error) {
	if raw == nil {
		return nil, nil
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return nil, err
	}
	return *(*bun.TableModel)(unsafe.Add(unsafe.Pointer(raw), layout.tableModel)), nil
}

func bunSelectQueryModel(raw *bun.SelectQuery) (bun.Model, error) {
	if raw == nil {
		return nil, nil
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return nil, err
	}
	return *(*bun.Model)(unsafe.Add(unsafe.Pointer(raw), layout.model)), nil
}

func sameBunModel(left, right bun.Model) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	if leftValue.Type().Comparable() {
		return leftValue.Interface() == rightValue.Interface()
	}

	// A non-comparable model value has no stable identity that can be checked
	// after a render-time callback. Fail closed rather than accepting a model
	// replacement that could change hooks, soft-delete policy, or event data.
	return false
}

// setBunSelectQueryEventModel preserves QueryEvent.Model without binding the
// model's physical table, relations, or soft-delete policy to a synthetic
// outer query. The complete model-aware SELECT remains the derived source.
func setBunSelectQueryEventModel(raw *bun.SelectQuery, model bun.Model) error {
	if raw == nil {
		return fmt.Errorf("sqldb: set query event model: query must not be nil")
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return err
	}
	*(*bun.Model)(unsafe.Add(unsafe.Pointer(raw), layout.model)) = model
	return nil
}

// prepareBunSelectCountSource removes clauses that Bun's Count terminal
// ignores from an already-cloned logical-result source. The caller owns raw;
// mutating it therefore cannot leak into the reusable public query builder.
func prepareBunSelectCountSource(raw *bun.SelectQuery) error {
	if raw == nil {
		return fmt.Errorf("sqldb: prepare count source: query must not be nil")
	}
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return err
	}
	ptr := unsafe.Pointer(raw)
	*(*[]schema.QueryWithArgs)(unsafe.Add(ptr, layout.order)) = nil
	*(*int32)(unsafe.Add(ptr, layout.limit)) = 0
	*(*int32)(unsafe.Add(ptr, layout.offset)) = 0
	*(*schema.QueryWithArgs)(unsafe.Add(ptr, layout.selFor)) = schema.QueryWithArgs{}
	return nil
}

// renderBunSelectCountSource renders the private source exactly once, then
// validates state that relation callbacks can mutate only during AppendQuery.
// The returned Safe SQL is the same validated render embedded by the outer
// count, preventing callbacks and custom appenders from running a second time.
func renderBunSelectCountSource(gen schema.QueryGen, raw *bun.SelectQuery) (bun.Safe, error) {
	if raw == nil {
		return "", fmt.Errorf("sqldb: render count source: query must not be nil")
	}
	beforeModel, err := bunSelectQueryModel(raw)
	if err != nil {
		return "", err
	}
	beforeTableModel, err := bunSelectQueryTableModel(raw)
	if err != nil {
		return "", err
	}
	query, err := raw.AppendQuery(gen, nil)
	if err != nil {
		return "", err
	}
	if queryErr := bunSelectQueryError(raw); queryErr != nil {
		return "", queryErr
	}
	afterModel, err := bunSelectQueryModel(raw)
	if err != nil {
		return "", err
	}
	afterTableModel, err := bunSelectQueryTableModel(raw)
	if err != nil {
		return "", err
	}
	if !sameBunModel(beforeModel, afterModel) || beforeTableModel != afterTableModel {
		return "", fmt.Errorf(
			"%w: relation callbacks must not replace the count source model",
			ErrUnsupportedCountQuery,
		)
	}
	if err := validateCountQueryShape(raw); err != nil {
		return "", err
	}
	if err := validateBunSelectCountSourceDecorations(raw); err != nil {
		return "", err
	}
	return bun.Safe(query), nil
}

func validateBunSelectCountSourceDecorations(raw *bun.SelectQuery) error {
	layout, err := loadBunSelectCloneLayout()
	if err != nil {
		return err
	}
	ptr := unsafe.Pointer(raw)
	order := bunSelectSliceLen(raw, layout.order)
	limit := *(*int32)(unsafe.Add(ptr, layout.limit))
	offset := *(*int32)(unsafe.Add(ptr, layout.offset))
	selFor := *(*schema.QueryWithArgs)(unsafe.Add(ptr, layout.selFor))
	if order == 0 && limit == 0 && offset == 0 && selFor.IsZero() {
		return nil
	}
	return fmt.Errorf(
		"%w: relation callbacks must not add root ORDER/LIMIT/OFFSET/FOR state "+
			"to a Count/Page source",
		ErrUnsupportedCountQuery,
	)
}

type bunSelectSliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

func bunSelectSliceLen(raw *bun.SelectQuery, offset uintptr) int {
	header := (*bunSelectSliceHeader)(unsafe.Add(unsafe.Pointer(raw), offset))
	return header.len
}

func patchBunSelectWithQueries(dst, src unsafe.Pointer, layout bunSelectCloneLayout) {
	source := *(*[]bun.WithQuery)(unsafe.Add(src, layout.with))
	target := *(*[]bun.WithQuery)(unsafe.Add(dst, layout.with))
	if len(target) != len(source) {
		panic(fmt.Sprintf(
			"sqldb: incompatible Bun SelectQuery.Clone WITH length: got %d, want %d",
			len(target),
			len(source),
		))
	}
	for i := range source {
		sourceWith := unsafe.Pointer(&source[i])
		targetWith := unsafe.Pointer(&target[i])
		copyBunSelectField[bool](targetWith, sourceWith, layout.withMaterialized)
		copyBunSelectField[bool](targetWith, sourceWith, layout.withNotMaterialized)
	}
}

func copyBunSelectField[T any](dst, src unsafe.Pointer, offset uintptr) {
	*(*T)(unsafe.Add(dst, offset)) = *(*T)(unsafe.Add(src, offset))
}

func cloneBunSelectSlice[T any](dst, src unsafe.Pointer, offset uintptr) {
	source := *(*[]T)(unsafe.Add(src, offset))
	*(*[]T)(unsafe.Add(dst, offset)) = slices.Clone(source)
}
