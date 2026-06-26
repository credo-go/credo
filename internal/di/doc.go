// Adapted from github.com/samber/do (MIT License).
// Original copyright (c) 2022 Samuel Berthe.
//
// Package di provides a type-safe dependency injection container using Go
// generics. All services use the Singleton lifecycle. It supports single
// resolution, interface aliases, and ordered interface collections.
//
// This package is internal to the Credo module. Use the public API in the
// root package: [credo.App.Provide], [credo.App.Resolve], etc.
package di
