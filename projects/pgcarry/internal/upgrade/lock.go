package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes the backup folder's lock, held for as long as an upgrade,
// rollback or cleanup works on it. The kernel drops it when the process
// ends, however it ends.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("a pgcarry run is still working on %s; wait for it to finish (or stop it) first", dir)
	}
	return func() { f.Close() }, nil
}
