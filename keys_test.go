package credo_test

import "github.com/credo-go/credo"

// Keys shared by the black-box tests of the root package.
var (
	keyAppLevel   = credo.NewKey[bool]("app-level")
	keyAuth       = credo.NewKey[bool]("auth")
	keyGlobal     = credo.NewKey[string]("global")
	keyGroupFlag  = credo.NewKey[bool]("group-flag")
	keyK          = credo.NewKey[string]("k")
	keyPermission = credo.NewKey[string]("permission")
	keyPublic     = credo.NewKey[bool]("public")
	keyScope      = credo.NewKey[string]("scope")
	keyStr        = credo.NewKey[string]("key")
	keyZone       = credo.NewKey[string]("zone")
)
