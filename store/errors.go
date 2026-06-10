package store

import "errors"

// statusError carries both the error message and an HTTP status code.
// The default error handler detects HTTPStatus() via errors.As without
// importing this package — no circular dependency.
type statusError struct {
	msg    string
	status int
}

func (e *statusError) Error() string   { return e.msg }
func (e *statusError) HTTPStatus() int { return e.status }

type classifiedError struct {
	kind   error
	cause  error
	status int
}

func (e *classifiedError) Error() string {
	return e.cause.Error()
}

func (e *classifiedError) Unwrap() error {
	return e.cause
}

func (e *classifiedError) HTTPStatus() int {
	return e.status
}

func (e *classifiedError) Is(target error) bool {
	return target == e.kind || errors.Is(e.cause, target)
}

// Sentinel errors for data access operations.
// Each error carries an HTTP status code accessible via HTTPStatus().
//
// errors.Is works via pointer identity. errors.As unwraps
// fmt.Errorf("%w", ...) chains to find the HTTPStatus() method.
var (
	// ErrNotFound indicates the requested record does not exist (HTTP 404).
	ErrNotFound error = &statusError{"store: record not found", 404}

	// ErrDuplicate indicates a unique constraint violation (HTTP 409).
	ErrDuplicate error = &statusError{"store: duplicate record", 409}

	// ErrConflict indicates an integrity-constraint violation other than a
	// duplicate (foreign key, not-null) or a transient transaction conflict
	// (serialization failure, deadlock — safe to retry) (HTTP 409).
	ErrConflict error = &statusError{"store: conflict", 409}

	// ErrTimeout indicates a database operation exceeded its deadline (HTTP 504).
	ErrTimeout error = &statusError{"store: timeout", 504}

	// ErrReadOnly indicates a write was attempted on a read-only connection (HTTP 503).
	ErrReadOnly error = &statusError{"store: read-only", 503}
)

// Wrap preserves the original cause while classifying it as one of the
// package sentinel errors. The returned error still matches the sentinel via
// errors.Is and still reports the sentinel's HTTP status via HTTPStatus().
func Wrap(kind error, cause error) error {
	if cause == nil {
		return nil
	}
	if kind == nil || errors.Is(cause, kind) {
		return cause
	}

	statusCarrier, ok := kind.(interface{ HTTPStatus() int })
	if !ok {
		return cause
	}

	return &classifiedError{
		kind:   kind,
		cause:  cause,
		status: statusCarrier.HTTPStatus(),
	}
}
