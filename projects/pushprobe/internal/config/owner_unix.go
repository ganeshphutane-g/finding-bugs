//go:build !windows

package config

import (
	"os"
	"syscall"
)

// keepOwner gives a new file the owner and group of the one it replaces,
// where allowed.
func keepOwner(old, new string) {
	if fi, err := os.Stat(old); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			os.Chown(new, int(st.Uid), int(st.Gid))
		}
	}
}
