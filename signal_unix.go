//go:build unix

package credo

import (
	"os"
	"syscall"
)

// reloadSignals lists the signals that trigger [App.Reload] under [App.Run].
// SIGHUP is the conventional "re-read your configuration" signal on Unix
// (systemctl reload, logrotate postrotate, nginx -s reload).
func reloadSignals() []os.Signal { return []os.Signal{syscall.SIGHUP} }
