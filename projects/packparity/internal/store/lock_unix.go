//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock takes the parity directory's lock, so two runs cannot change it at
// once, waiting up to wait for another run to finish. It is released by
// unlock, or when the process ends.
func (s *Store) Lock(wait time.Duration) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(s.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another packparity is still working on %s (waited %s; see --wait)", s.Root, wait)
		}
		time.Sleep(time.Second)
	}
}
