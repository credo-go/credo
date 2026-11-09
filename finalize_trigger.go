package credo

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"github.com/credo-go/credo/internal/di"
)

// triggerCallers is how many callers of a trigger's site are recorded.
const triggerCallers = 3

// remedyBeforeCall is the remedy of a trigger that has a call to point at.
const remedyBeforeCall = "register before that call"

// callTrigger is the trigger of call — Finalize, a resolving call with its
// type, or an entry point such as Run — made at the first frame outside
// Credo's packages and testutil, with up to three of that frame's callers
// outside them and the standard library: when a helper library makes the
// call, the site is the helper's line and the callers name the code that
// called it.
func callTrigger(call string) di.Trigger {
	return di.Trigger{By: call + renderTriggerSite(), Remedy: remedyBeforeCall}
}

// serveHTTPTrigger is the trigger of the first ServeHTTP call, which has no
// site: its caller is net/http.
var serveHTTPTrigger = di.Trigger{By: "the first ServeHTTP call", Remedy: "register before the App serves"}

// renderTriggerSite renders the site of the call being made, " at file:line
// (called from file:line, …)", or "" when every frame is Credo's.
func renderTriggerSite() string {
	var pcs [64]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var site string
	var callers []string
	for {
		frame, more := frames.Next()
		switch {
		case frame.Function == "" || isTriggerFrameworkFrame(frame.Function):
		case site == "":
			site = sourceLocation{file: frame.File, line: frame.Line}.String()
		case !isStdlibFrame(frame.Function, frame.File):
			callers = append(callers, sourceLocation{file: frame.File, line: frame.Line}.String())
		}
		if !more || len(callers) == triggerCallers {
			break
		}
	}
	if site == "" {
		return ""
	}
	if len(callers) == 0 {
		return " at " + site
	}
	return fmt.Sprintf(" at %s (called from %s)", site, strings.Join(callers, ", "))
}

// isTriggerFrameworkFrame reports whether fn belongs to one of Credo's
// packages — its root, internal, testutil and public subpackages, but not a
// black-box test package or an example — so that a trigger's site points at
// the application, or at the test that called a testutil helper.
func isTriggerFrameworkFrame(fn string) bool {
	const base = "github.com/credo-go/credo"
	pkg := framePackage(fn)
	if pkg != base && !strings.HasPrefix(pkg, base+"/") {
		return false
	}
	return !strings.HasSuffix(pkg, "_test") && !strings.HasPrefix(pkg, base+"/examples/")
}

// framePackage returns the import path of the package a fully-qualified
// function name belongs to: "net/http.(*conn).serve" → "net/http".
func framePackage(fn string) string {
	slash := strings.LastIndexByte(fn, '/')
	if dot := strings.IndexByte(fn[slash+1:], '.'); dot >= 0 {
		return fn[:slash+1+dot]
	}
	return fn
}

// isStdlibFrame reports whether a frame belongs to the standard library: its
// file lies under GOROOT's source tree when that tree is known from the
// binary, and otherwise its package's first path element has no dot and is
// not main.
func isStdlibFrame(fn, file string) bool {
	if root := gorootSource(); root != "" {
		return strings.HasPrefix(file, root)
	}
	pkg := framePackage(fn)
	first, _, _ := strings.Cut(pkg, "/")
	return pkg != "main" && !strings.Contains(first, ".")
}

// gorootSource is GOROOT's source directory with its trailing slash, as the
// binary records it in the file of a standard-library function, or "" when
// that file is not under a src directory (a -trimpath build).
var gorootSource = sync.OnceValue(func() string {
	fn := runtime.FuncForPC(reflect.ValueOf(fmt.Sprint).Pointer())
	if fn == nil {
		return ""
	}
	file, _ := fn.FileLine(fn.Entry())
	root, ok := strings.CutSuffix(file, "fmt/print.go")
	if !ok || !strings.HasSuffix(root, "/src/") {
		return ""
	}
	return root
})
