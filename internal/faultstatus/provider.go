package faultstatus

import "errors"

// StatusProvider is implemented by errors that carry an explicit HTTP status.
// It is the transport-side counterpart of fault.Provider: a fault reports a
// semantic kind that HTTP maps to a default status, while a StatusProvider
// names the status directly (root HTTPError, BindError, store legacy errors).
//
// The interface lives here so the root package and internal/observe share one
// definition; observe cannot import the root package.
type StatusProvider interface {
	error
	HTTPStatus() int
}

// ProviderOf extracts the first StatusProvider in err's chain.
func ProviderOf(err error) (StatusProvider, bool) {
	return errors.AsType[StatusProvider](err)
}
