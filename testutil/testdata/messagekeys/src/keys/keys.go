package keys

import "github.com/credo-go/credo"

// TenantTaken is defined in the catalogs.
const TenantTaken credo.MessageKey = "tenant.taken"

// Gone is a typed key the catalogs do not define.
const Gone credo.MessageKey = "keys.gone"

// StatusPrefix begins a family the catalogs define keys under.
const StatusPrefix = "status."

// OrphanPrefix begins a family no catalog has a key under.
const OrphanPrefix = "orphan" + "."
