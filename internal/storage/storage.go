// Package storage handles qcow2 disk operations and pool management.
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// FileMode is the mode of every disk and seed. On Linux libvirt chowns each
// to QEMU's user while its domain runs, so no other account needs to read it.
const FileMode os.FileMode = 0o600

// DirMode is for the directories nano-cloud creates for VM state. Others keep
// the search bit QEMU needs to open a disk by path, as on /var/lib/libvirt/images.
const DirMode os.FileMode = 0o711

// Restrict takes group and other access off the regular file at path and
// reports whether it changed anything. A missing path is not an error.
func Restrict(path string) (bool, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 == 0 {
		return false, nil
	}
	if err := os.Chmod(path, st.Mode().Perm()&^0o077); err != nil {
		return false, fmt.Errorf("storage: %w", err)
	}
	return true, nil
}

// DiskPathFor and ISOPathFor define the disk and seed ISO naming convention in
// one place.
func DiskPathFor(poolPath, name string) string {
	return filepath.Join(poolPath, name+DiskExt)
}

func ISOPathFor(poolPath, name string) string {
	return filepath.Join(poolPath, name+"-cidata.iso")
}

// PathsFor returns the two files a domain definition names, or ok=false when
// the VM records no pool. An empty pool must be refused: filepath.Join would
// return a relative path, which resolves against the daemon's cwd.
func PathsFor(poolPath, name string) (disk, iso string, ok bool) {
	if poolPath == "" {
		return "", "", false
	}
	return DiskPathFor(poolPath, name), ISOPathFor(poolPath, name), true
}

// FreeSpace reports the bytes available to an unprivileged process and the
// total capacity of the filesystem at path.
func FreeSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Block counts are in f_frsize units on Linux, not f_bsize.
	blockSize := blockSizeForStatfs(st)
	free = st.Bavail * blockSize
	total = st.Blocks * blockSize
	return free, total, nil
}

// DiskInfo is the part of `qemu-img info` a caller has to decide on.
type DiskInfo struct {
	// VirtualSizeBytes is the size the guest sees, not the space used on the host.
	VirtualSizeBytes int64
	// Snapshots is the number of internal snapshots in the image. An image
	// with any cannot be resized.
	Snapshots int
}

// BytesPerGB is the gibibyte that qemu-img's "G" suffix means.
const BytesPerGB = 1024 * 1024 * 1024

// DeleteDisk removes the disk file at path. A missing file is not an error.
func DeleteDisk(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete disk %q: %w", path, err)
	}
	removeSidecars(path)
	return nil
}
