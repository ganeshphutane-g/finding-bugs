package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lock makes sure only one pushdir watches a folder at a time: two would
// both copy and delete, and judge each other's deletions.
func lock(local string) (func(), error) {
	real, err := filepath.EvalSymlinks(local)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(real))
	path := filepath.Join(os.TempDir(), fmt.Sprintf("pushdir-%x.lock", sum[:8]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another pushdir is already watching %s (lock %s)", local, path)
	}
	return func() { f.Close() }, nil
}
