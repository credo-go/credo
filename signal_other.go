//go:build !unix

package credo

import "os"

// reloadSignals reports no reload signals: there is no SIGHUP on this
// platform, so [App.Reload] is the only reload trigger.
func reloadSignals() []os.Signal { return nil }
