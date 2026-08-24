//go:build !windows

package server

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// ContextWithSignals returns a context canceled by the normal process stop
// signals. Container runtimes send SIGTERM, while local interactive plugins
// normally receive os.Interrupt.
func ContextWithSignals(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
