//go:build !linux

package steps

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Mkdir creates each directory (with parents) if missing, then gives it the
// owner and mode, when set and different. Relative paths are from dir.
func Mkdir(r Report, dir string, paths []string, owner *Owner, mode *fs.FileMode) error {
	for _, p := range paths {
		if p == "" {
			return errors.New("mkdir: an empty path")
		}
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(dir, p)
		}
		full = filepath.Clean(full)
		st, err := os.Lstat(full)
		switch {
		case err == nil && st.Mode()&fs.ModeSymlink != 0:
			// Never chown or chmod through a link: it may point anywhere.
			return fmt.Errorf("mkdir %s: it is a symbolic link; give the real directory", p)
		case err == nil && !st.IsDir():
			return fmt.Errorf("mkdir %s: it exists and is not a directory", p)
		case errors.Is(err, fs.ErrNotExist):
			// Parents get the usual 0755; the owner and mode are the
			// directory's own.
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", p, err)
			}
			m := fs.FileMode(0o755)
			if mode != nil {
				m = *mode
			}
			if err := os.Mkdir(full, m); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("mkdir %s: %w", p, err)
			}
			r.Info("created %s", p)
			if st, err = os.Lstat(full); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("mkdir %s: %w", p, err)
		}
		if owner != nil {
			uid, gid := ownerOf(st)
			want := *owner
			if want.UID == -1 {
				want.UID = uid
			}
			if want.GID == -1 {
				want.GID = gid
			}
			if uid != want.UID || gid != want.GID {
				if err := os.Lchown(full, want.UID, want.GID); err != nil {
					return fmt.Errorf("mkdir %s: owner %d:%d: %w (composehost runs as the user running Compose)", p, want.UID, want.GID, err)
				}
				r.Info("%s owned by %d:%d", p, want.UID, want.GID)
			}
		}
		if mode != nil && st.Mode().Perm() != *mode {
			// MkdirAll's mode is cut by the umask; this sets it exactly.
			if err := os.Chmod(full, *mode); err != nil {
				return fmt.Errorf("mkdir %s: mode %04o: %w", p, uint32(*mode), err)
			}
			r.Info("%s mode %04o", p, uint32(*mode))
		}
	}
	return nil
}
