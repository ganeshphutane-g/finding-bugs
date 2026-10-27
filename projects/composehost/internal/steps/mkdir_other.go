//go:build !linux

package steps

import (
	"errors"
	"io/fs"
)

// Mkdir needs Linux (openat and O_NOFOLLOW, for safety as root), as does
// the provider.
func Mkdir(r Report, dir string, paths []string, owner *Owner, mode *fs.FileMode) error {
	if len(paths) == 0 {
		return nil
	}
	return errors.New("mkdir: composehost runs on Linux only")
}
