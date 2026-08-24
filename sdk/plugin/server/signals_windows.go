//go:build windows

package server

import (
	"context"
	"os"
	"os/signal"
)

// ContextWithSignals returns a context canceled by an interactive Windows
// interrupt. Windows does not expose a portable SIGTERM equivalent.
func ContextWithSignals(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt)
}
