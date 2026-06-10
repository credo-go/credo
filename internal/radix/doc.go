// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi (MIT License).

// Package radix implements a compressed radix tree (Patricia trie) for HTTP
// request routing. It supports named parameters ({id}), regex-constrained
// parameters ({id:[0-9]+}), and catch-all parameters ({path...}).
//
// The tree is generic over its payload: Node[V] stores opaque values of
// type V at its endpoints and never inspects or invokes them — the router
// that owns the tree defines what a payload means.
//
// This package is internal to the Credo module and should not be imported
// directly by external consumers. Use the router package instead.
package radix
