package storage

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/lima-vm/go-qcow2reader"
	"github.com/lima-vm/go-qcow2reader/convert"
	"github.com/lima-vm/go-qcow2reader/image/raw"
)

// Raw disks, for Virtualization.framework, which boots raw images only.
// disk_darwin.go exposes these on macOS; they are untagged so they are built
// and tested everywhere.
//
// A VM's disk is an APFS clone of a raw conversion of the base image. Unlike a
// qcow2 overlay it depends on nothing afterwards, so the cached image can be
// deleted while VMs built from it keep running.

// rawMu serialises base-image conversion so concurrent creates convert once.
var rawMu sync.Mutex

// RawBasePath is where the raw conversion of a cached qcow2 image is kept, in a
// hidden directory the image cache's listing skips.
func RawBasePath(imagePath string) string {
	return filepath.Join(filepath.Dir(imagePath), ".raw", filepath.Base(imagePath)+".raw")
}

// rawBase returns a raw image with imagePath's contents: imagePath itself when
// it is already raw, otherwise a conversion that is redone whenever the image
// is newer than it.
func rawBase(imagePath string) (string, error) {
	rawMu.Lock()
	defer rawMu.Unlock()

	f, err := os.Open(imagePath)
	if err != nil {
		return "", fmt.Errorf("storage: open base image: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	src, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("storage: stat base image: %w", err)
	}

	img, err := qcow2reader.Open(f)
	if err != nil {
		return "", fmt.Errorf("storage: read base image %s: %w", imagePath, err)
	}
	defer img.Close() //nolint:errcheck // read-only
	if img.Type() == raw.Type {
		return imagePath, nil
	}
	if rerr := img.Readable(); rerr != nil {
		return "", fmt.Errorf("storage: base image %s (%s) cannot be converted: %w", imagePath, img.Type(), rerr)
	}

	out := RawBasePath(imagePath)
	if st, serr := os.Stat(out); serr == nil && !st.ModTime().Before(src.ModTime()) {
		return out, nil
	}
	if err = os.MkdirAll(filepath.Dir(out), DirMode); err != nil {
		return "", fmt.Errorf("storage: %w", err)
	}
	tmp := out + ".tmp"
	_ = os.Remove(tmp)
	w, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("storage: %w", err)
	}
	// Only non-zero ranges are written, so the conversion is sparse.
	if err := convert.Convert(w, img, convert.Options{}); err != nil {
		w.Close()      //nolint:errcheck // already failing
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("storage: convert %s to raw: %w", imagePath, err)
	}
	if err := w.Truncate(img.Size()); err != nil {
		w.Close()      //nolint:errcheck // already failing
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("storage: size raw base: %w", err)
	}
	if err := w.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("storage: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", fmt.Errorf("storage: %w", err)
	}
	return out, nil
}

// allocateRaw makes destPath a sizeGB disk holding backingFile's contents.
func allocateRaw(backingFile, destPath string, sizeGB int) error {
	base, err := rawBase(backingFile)
	if err != nil {
		return err
	}
	st, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	want := int64(sizeGB) * BytesPerGB
	if want < st.Size() {
		return fmt.Errorf("storage: a %d GiB disk cannot hold this image, which needs %d GiB", sizeGB, (st.Size()+BytesPerGB-1)/BytesPerGB)
	}
	if err := cloneOrCopy(base, destPath); err != nil {
		return err
	}
	if err := os.Truncate(destPath, want); err != nil {
		os.Remove(destPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("storage: size disk: %w", err)
	}
	return nil
}

// createRaw makes a blank, sparse sizeGB disk at path.
func createRaw(path string, sizeGB int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("storage: create disk: %w", err)
	}
	if err := f.Truncate(int64(sizeGB) * BytesPerGB); err != nil {
		f.Close()       //nolint:errcheck // already failing
		os.Remove(path) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("storage: size disk: %w", err)
	}
	return f.Close()
}

// copyRaw writes a standalone copy of srcPath to destPath. The source must not
// be in use by a running VM.
func copyRaw(srcPath, destPath string) error {
	return cloneOrCopy(srcPath, destPath)
}

// resizeRaw grows the disk at path to sizeGB. It refuses to shrink.
func resizeRaw(path string, sizeGB int) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	want := int64(sizeGB) * BytesPerGB
	if want < st.Size() {
		return fmt.Errorf("storage: refusing to shrink %s from %d to %d bytes", path, st.Size(), want)
	}
	if err := os.Truncate(path, want); err != nil {
		return fmt.Errorf("storage: resize disk: %w", err)
	}
	return nil
}

// inspectRaw reports a raw disk's size and snapshot count. Raw snapshots live
// in SnapshotDir; counting them applies the "no resize with snapshots" rule
// the same way as for qcow2.
func inspectRaw(path string) (DiskInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return DiskInfo{}, fmt.Errorf("storage: %w", err)
	}
	snaps, _ := filepath.Glob(filepath.Join(SnapshotDir(path), "*"+DiskExt))
	return DiskInfo{VirtualSizeBytes: st.Size(), Snapshots: len(snaps)}, nil
}

// SnapshotDir is where a raw disk's snapshots are kept, one copy-on-write
// clone per snapshot.
func SnapshotDir(diskPath string) string { return diskPath + ".snapshots" }

// CloneOrCopy makes dst a FileMode copy of src: an instant copy-on-write clone
// where the file system can (APFS), a sparse copy otherwise. dst must not
// exist.
func CloneOrCopy(src, dst string) error { return cloneOrCopy(src, dst) }

// SyncFile flushes a file's cached writes to disk so a following clone
// includes them.
func SyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return f.Sync()
}

// cloneOrCopy builds the copy in a private directory and links it into place
// once it is FileMode: a clone starts with src's mode, and a descriptor opened
// before the chmod would see every later write. A link, unlike a rename,
// refuses an existing dst.
func cloneOrCopy(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("storage: %s already exists", dst)
	}
	stage, err := os.MkdirTemp(filepath.Dir(dst), ".clone-")
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer os.RemoveAll(stage) //nolint:errcheck // best-effort; dst is a link of its own
	tmp := filepath.Join(stage, filepath.Base(dst))
	if err := clonefile(src, tmp); err != nil {
		if err := sparseCopy(src, tmp); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, FileMode); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if err := os.Link(tmp, dst); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	return nil
}

// sparseCopy copies src to dst, leaving holes where src reads as zeros.
func sparseCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only
	st, err := in.Stat()
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	fail := func(err error) error {
		out.Close()    //nolint:errcheck // already failing
		os.Remove(dst) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("storage: copy %s: %w", src, err)
	}
	buf := make([]byte, 1<<20)
	zero := make([]byte, len(buf))
	var off int64
	for {
		n, rerr := io.ReadFull(in, buf)
		if n > 0 && !bytes.Equal(buf[:n], zero[:n]) {
			if _, err := out.WriteAt(buf[:n], off); err != nil {
				return fail(err)
			}
		}
		off += int64(n)
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return fail(rerr)
		}
	}
	if err := out.Truncate(st.Size()); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("storage: %w", err)
	}
	return nil
}
