//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the parity directory's lock, so two runs cannot change it at
// once. It is released by unlock, or when the process ends.
func (s *Store) Lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(s.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another packparity is working on %s", s.Root)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
