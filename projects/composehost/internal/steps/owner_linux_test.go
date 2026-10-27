package steps

import (
	"io/fs"
	"syscall"
)

func ownerOf(st fs.FileInfo) (uid, gid int) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid), int(s.Gid)
	}
	return -1, -1
}
