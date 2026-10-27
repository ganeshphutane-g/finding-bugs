package steps

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Mkdir creates each directory (with parents) if missing, then gives it the
// owner and mode, when set and different. Relative paths are from dir.
//
// composehost often runs as root, on directories that containers (other
// users) can change. So it walks each path one directory at a time, with
// openat and O_NOFOLLOW, and sets the owner and mode on the open directory:
//   - a symbolic link is followed only when the directory holding it is
//     root's or composehost's own user's, and writable by no one else;
//     anywhere else it is refused, so a link swapped in cannot redirect a
//     chown or chmod;
//   - a directory it creates gets exactly its mode, whatever the umask
//     (0755 for parents).
func Mkdir(r Report, dir string, paths []string, owner *Owner, mode *fs.FileMode) error {
	for _, p := range paths {
		if p == "" {
			return errors.New("mkdir: an empty path")
		}
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(dir, p)
		}
		if err := mkdirOne(r, p, filepath.Clean(full), owner, mode); err != nil {
			return fmt.Errorf("mkdir %s: %w", p, err)
		}
	}
	return nil
}

// system are directories whose owner and mode composehost never changes
// (an unset variable can make a path /), by device and inode: a path may
// reach one through links.
var system = map[[2]uint64]string{}

func init() {
	for _, d := range []string{"/", "/bin", "/boot", "/dev", "/dev/shm", "/etc", "/home", "/lib", "/lib32",
		"/lib64", "/media", "/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr",
		"/usr/bin", "/usr/include", "/usr/lib", "/usr/lib32", "/usr/lib64", "/usr/libexec", "/usr/local",
		"/usr/local/bin", "/usr/local/lib", "/usr/local/sbin", "/usr/sbin", "/usr/share", "/usr/src", "/var",
		"/var/cache", "/var/lib", "/var/lib/docker", "/var/log", "/var/mail", "/var/run", "/var/spool",
		"/var/tmp", "/etc/ssh", "/etc/systemd", "/etc/sudoers.d", "/etc/cron.d"} {
		var st unix.Stat_t
		if unix.Stat(d, &st) == nil {
			system[[2]uint64{uint64(st.Dev), st.Ino}] = d
		}
	}
}

const dirFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC

func mkdirOne(r Report, p, full string, owner *Owner, mode *fs.FileMode) error {
	leafMode := uint32(0o755)
	if mode != nil {
		leafMode = uint32(*mode)
	}
	fd, err := syscall.Open("/", dirFlags, 0)
	if err != nil {
		return err
	}
	defer func() { syscall.Close(fd) }()
	at := "/" // where fd is, for messages
	comps := split(full)
	links := 0
	created := false
	for len(comps) > 0 {
		c := comps[0]
		comps = comps[1:]
		leaf := len(comps) == 0
		var st unix.Stat_t
		err := unix.Fstatat(fd, c, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, syscall.ENOENT):
			m := uint32(0o755)
			if leaf {
				m = leafMode
			}
			made := true
			if err := syscall.Mkdirat(fd, c, m); errors.Is(err, syscall.EEXIST) {
				made = false // made by someone else meanwhile: looked at below
			} else if err != nil {
				return err
			}
			nfd, err := openDir(fd, c, at)
			if err != nil {
				return err
			}
			if made {
				// Mkdirat's mode is cut by the umask; this sets it exactly.
				if err := syscall.Fchmod(nfd, m); err != nil {
					syscall.Close(nfd)
					return err
				}
				created = created || leaf
			}
			syscall.Close(fd)
			fd, at = nfd, filepath.Join(at, c)
		case err != nil:
			return err
		case st.Mode&unix.S_IFMT == unix.S_IFLNK:
			if !trusted(fd) {
				return fmt.Errorf("%s is a symbolic link in a directory that other users can change; composehost does not follow those (give the real path)", filepath.Join(at, c))
			}
			if links++; links > 40 {
				return syscall.ELOOP
			}
			target, err := readlinkat(fd, c)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				nfd, err := syscall.Open("/", dirFlags, 0)
				if err != nil {
					return err
				}
				syscall.Close(fd)
				fd, at = nfd, "/"
			}
			comps = append(split(target), comps...)
		case st.Mode&unix.S_IFMT != unix.S_IFDIR:
			return errors.New(filepath.Join(at, c) + " exists and is not a directory")
		default:
			nfd, err := openDir(fd, c, at)
			if err != nil {
				return err
			}
			syscall.Close(fd)
			fd, at = nfd, filepath.Join(at, c)
		}
	}
	if created {
		r.Info("created %s", p)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if d, ok := system[[2]uint64{uint64(st.Dev), st.Ino}]; ok && changes(&st, owner, mode) {
		return fmt.Errorf("composehost does not change the owner or mode of %s", d)
	}
	if owner != nil {
		uid, gid := int(st.Uid), int(st.Gid)
		want := *owner
		if want.UID == -1 {
			want.UID = uid
		}
		if want.GID == -1 {
			want.GID = gid
		}
		if uid != want.UID || gid != want.GID {
			if err := syscall.Fchown(fd, want.UID, want.GID); err != nil {
				return fmt.Errorf("owner %d:%d: %w (composehost runs as the user running Compose)", want.UID, want.GID, err)
			}
			r.Info("%s owned by %d:%d", p, want.UID, want.GID)
		}
	}
	if mode != nil && st.Mode&0o7777 != uint32(*mode) {
		if err := syscall.Fchmod(fd, uint32(*mode)); err != nil {
			return fmt.Errorf("mode %04o: %w", uint32(*mode), err)
		}
		r.Info("%s mode %04o", p, uint32(*mode))
	}
	return nil
}

func readlinkat(fd int, c string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(fd, c, buf)
	if err != nil {
		return "", err
	}
	if n == len(buf) {
		return "", errors.New(c + ": the link is too long")
	}
	return string(buf[:n]), nil
}

// changes says whether the owner or mode would change.
func changes(st *syscall.Stat_t, owner *Owner, mode *fs.FileMode) bool {
	if owner != nil && (owner.UID != -1 && owner.UID != int(st.Uid) || owner.GID != -1 && owner.GID != int(st.Gid)) {
		return true
	}
	return mode != nil && st.Mode&0o7777 != uint32(*mode)
}

// openDir opens the directory c in fd, refusing a link swapped in.
func openDir(fd int, c, at string) (int, error) {
	nfd, err := syscall.Openat(fd, c, dirFlags, 0)
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return -1, errors.New(filepath.Join(at, c) + " changed while composehost looked at it (is something swapping it for a link?)")
	}
	return nfd, err
}

// trusted says whether links in the open directory fd can be followed:
// it is root's or ours, and no one else can write to it.
func trusted(fd int) bool {
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return false
	}
	return (st.Uid == 0 || int(st.Uid) == os.Geteuid()) && st.Mode&0o022 == 0
}

// split is a path's names, without empty ones and ".".
func split(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}
