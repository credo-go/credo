// Package worker is the engine behind the root package's workers: the run
// loop of each kind, the restart backoff, the run outcome classification, the
// schedule loop and the cron parser. It does not import the root package; the
// root's Workers registry validates registrations, converts its public types
// at the boundary and hands each worker's Component to the App as a lifecycle
// component named "worker:<name>".
//
// # Adapted From
//
// Cron expression parsing and next-fire calculation are adapted from
// robfig/cron v3 (MIT). See NOTICES for full attribution.
package worker
