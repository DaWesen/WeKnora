package main

import (
	"net"
)

// dialTCP attempts a TCP connection without an explicit timeout — callers
// wrap the loop in a context.
func dialTCP(addr string) (net.Conn, error) {
	return net.Dial("tcp", addr)
}

// dialUnix attempts a unix socket connection.
func dialUnix(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}
