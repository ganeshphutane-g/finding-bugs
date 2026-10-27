//go:build !linux

package steps

import "io/fs"

func ownerOf(st fs.FileInfo) (uid, gid int) { return -1, -1 }
