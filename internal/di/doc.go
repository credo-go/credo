// Adapted from github.com/samber/do (MIT License).
// Original copyright (c) 2022 Samuel Berthe.
//
// Package di provides a type-safe dependency injection container using Go
// generics. All services use the Singleton lifecycle. It supports single
// resolution, interface aliases, and ordered interface collections.
//
// The container has three phases. During registration, bindings are written
// and prebuilt values may be adopted by integrations; constructors never run.
// Seal freezes the registrations and validates the graph, and only then is
// Resolve admitted. Shutdown enters the closing phase: resolution is rejected
// with [ErrClosed] and instances are torn down consumers-before-dependencies,
// with the outcome reported as a [ShutdownError] snapshot.
//
// This package is internal to the Credo module. Use the public API in the
// root package: [credo.App.Provide], [credo.App.Resolve], etc.
package di
