// Package fault defines transport-neutral semantic errors: a [Kind], the
// [Provider] interface that exposes one, and [Error], the error a domain layer
// returns without naming a transport status.
//
// Feature packages expose a Kind through Provider without importing the HTTP
// or future gRPC policy layers, and a transport maps the same kind to its own
// status. A fault without a code of its own is rendered with its kind as the
// code. A service that names its own code returns an [Error]:
//
//	var ErrDomainTaken = fault.New(fault.KindAlreadyExists, "tenant_domain_taken")
//
// The handler returns it unchanged; the transport gives it the kind's status,
// renders its code and details, and logs its internal cause. A status that
// only the HTTP boundary knows stays a credo.HTTPError.
//
// The package carries no status and no message key: the client message is
// resolved from the code by the transport.
//
// Maturity: beta
package fault
