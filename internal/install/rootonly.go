package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// CheckBinPath returns an error naming the fix unless only root can change
// BinPath and every directory above it.
func CheckBinPath() error { return checkRootOnly(binDst, hostFS{}) }

// ownerFS is the part of the host checkRootOnly reads, so it can be tested
// without root.
type ownerFS interface {
	// owner reports path's owner, group and permission bits, following symlinks.
	owner(path string) (uid, gid uint32, perm fs.FileMode, err error)
	resolve(path string) (string, error)
	// staffGID reports the gid of the staff group when its write access is
	// accepted on this platform.
	staffGID() (gid uint32, ok bool)
}

type hostFS struct{}

func (hostFS) owner(path string) (uint32, uint32, fs.FileMode, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, fmt.Errorf("stat %s: no owner reported", path)
	}
	return st.Uid, st.Gid, fi.Mode().Perm(), nil
}

func (hostFS) resolve(path string) (string, error) { return filepath.EvalSymlinks(path) }

// staffGID looks staff up by name, since its gid differs between systems. A
// failed lookup trusts no group, so a group-writable directory is refused.
func (hostFS) staffGID() (uint32, bool) {
	if !trustStaff {
		return 0, false
	}
	g, err := user.LookupGroup("staff")
	if err != nil {
		return 0, false
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(gid), true
}

// checkRootOnly refuses path unless only root can change what it names. The
// symlink-resolved path is walked too, since its parent directories differ.
func checkRootOnly(path string, fsys ownerFS) error {
	if err := rootOnlyChain(path, fsys); err != nil {
		return err
	}
	resolved, err := fsys.resolve(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("resolve %s: %w", path, err)
	case resolved == path:
		return nil
	}
	return rootOnlyChain(resolved, fsys)
}

// rootOnlyChain returns an error naming the first of path and its parents that
// is not owned by root, is writable by others, or is writable by a group other
// than staff where staff is trusted. A sticky bit does not excuse a writable
// directory: others could still plant a file under a name root writes next,
// such as the .prev copy. Missing components are skipped, since install
// creates them as root.
func rootOnlyChain(path string, fsys ownerFS) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		uid, gid, perm, err := fsys.owner(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return fmt.Errorf("check owner of %s: %w", p, err)
		case uid != 0:
			return unsafePathError(p, fmt.Sprintf("is owned by uid %d, not root", uid))
		case perm&0o002 != 0:
			return unsafePathError(p, fmt.Sprintf("is writable by others (mode %03o)", uint32(perm)))
		case perm&0o020 != 0 && !isStaff(gid, fsys):
			return unsafePathError(p, fmt.Sprintf("is writable by group %d (mode %03o)", gid, uint32(perm)))
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

func isStaff(gid uint32, fsys ownerFS) bool {
	staff, ok := fsys.staffGID()
	return ok && gid == staff
}

// unsafePathError gives the fix rather than applying it, because Homebrew may
// own /usr/local/bin on a Mac and re-owning it breaks brew.
func unsafePathError(path, problem string) error {
	note := ""
	if trustStaff {
		note = " Group write is accepted only for staff, which Debian treats as equivalent to root."
	}
	return fmt.Errorf("%s %s.\n"+
		"%s runs %s as root, so anyone who can modify %s can swap in their own binary and gain root at the next restart.%s\n"+
		"Fix it, then try again:\n"+
		"  sudo chown root:%s %s && sudo chmod 755 %s",
		path, problem, serviceManager, binDst, path, note, rootGroup, path, path)
}
