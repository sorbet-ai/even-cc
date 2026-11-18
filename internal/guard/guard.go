// Package guard keeps a panic in one goroutine from taking the whole server
// down: it is logged with its stack and the goroutine ends.
package guard

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
)

// Panics counts recovered panics (shown by /ecc status and in tests).
var Panics atomic.Int64

// Recover logs a panic and swallows it. Use it directly as the deferred
// call: defer guard.Recover(log, "what"). log may be nil.
func Recover(log *slog.Logger, what string) {
	if r := recover(); r != nil {
		Report(log, what, r)
	}
}

// Report logs a recovered panic value with the current stack.
func Report(log *slog.Logger, what string, r any) {
	Panics.Add(1)
	if log == nil {
		log = slog.Default()
	}
	log.Error("panic recovered; even-cc keeps running", "in", what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
}

// Go runs f in a goroutine that recovers from panics.
func Go(log *slog.Logger, what string, f func()) {
	go func() {
		defer Recover(log, what)
		f()
	}()
}
