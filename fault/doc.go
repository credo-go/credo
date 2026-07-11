// Package fault defines transport-neutral semantic error kinds.
//
// Feature packages expose a Kind through Provider without importing the HTTP
// or future gRPC policy layers. Transports can then map the same semantic kind
// independently while domain code can override that default at its boundary.
package fault
