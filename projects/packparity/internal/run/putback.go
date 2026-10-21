package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ganeshphutane-g/packparity/internal/repo"
)

// putBack replaces a repository file with its rebuilt contents in one
// rename, keeping its mode and owner. The folder is resolved once (a
// top-level folder may be a symlink the admin set up) and everything after
// that goes through its handle, without following symlinks, so a folder
// swapped during the repair cannot redirect the write.
func putBack(root, name string, fixed []byte) error {
	dir, base := path.Split(name)
	real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(dir, "/"))))
	if err != nil {
		return err
	}
	dfd, err := unix.Open(real, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", real, err)
	}
	defer unix.Close(dfd)
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "stat", Path: name, Err: err}
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s is not a regular file", name)
	}
	uid, gid := int(st.Uid), int(st.Gid)
	if me := os.Geteuid(); me != 0 && me != uid {
		return fmt.Errorf("%s belongs to user %d; repair it as that user or as root, so it keeps its owner", name, uid)
	}
	rb := make([]byte, 8)
	rand.Read(rb)
	tmp := repo.TempPrefix + hex.EncodeToString(rb)
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", real, err)
	}
	ok := false
	defer func() {
		if !ok {
			unix.Unlinkat(dfd, tmp, 0)
		}
	}()
	f := os.NewFile(uintptr(fd), tmp)
	defer f.Close()
	if _, err := f.Write(fixed); err != nil {
		return err
	}
	if err := unix.Fchown(fd, uid, gid); err != nil && os.Geteuid() == 0 {
		return fmt.Errorf("keeping the owner: %w", err)
	}
	if err := unix.Fchmod(fd, uint32(st.Mode)&0o7777); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Renameat(dfd, tmp, dfd, base); err != nil {
		return err
	}
	ok = true
	unix.Fsync(dfd)
	return nil
}
