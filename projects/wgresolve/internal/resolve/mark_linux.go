package resolve

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// markControl sets SO_MARK on a socket before it connects (needs
// CAP_NET_ADMIN, which wgresolve has).
func markControl(mark int) func(network, address string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
		})
		if err != nil {
			return err
		}
		return serr
	}
}
