//go:build !linux

package resolve

import "syscall"

func markControl(mark int) func(network, address string, c syscall.RawConn) error { return nil }
