//go:build !darwin

package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// The disk operations below shell out to qemu-img and produce qcow2.
// disk_darwin.go implements them with raw images for Virtualization.framework.

// AllocateDisk creates a qcow2 overlay at destPath backed by backingFile.
// sizeGB is the virtual size in gibibytes and must be at least the backing
// file's virtual size.
func AllocateDisk(backingFile, destPath string, sizeGB int) error {
	return newImage(destPath, "create", "-f", "qcow2", "-b", backingFile, "-F", "qcow2", destPath, sizeGiB(sizeGB))
}

// CreateDisk creates an empty qcow2 disk at path, with no backing file. It is
// never raw: a raw disk anywhere in a domain makes libvirt refuse internal
// snapshots of the whole VM.
func CreateDisk(path string, sizeGB int) error {
	return newImage(path, "create", "-f", "qcow2", path, sizeGiB(sizeGB))
}

// CopyDisk writes a standalone copy of the disk at srcPath to destPath. It uses
// qemu-img convert to flatten the backing chain, so the copy does not depend
// on a cached base image that may later be deleted. The source VM must be
// stopped.
func CopyDisk(srcPath, destPath string) error {
	return newImage(destPath, "convert", "-O", "qcow2", srcPath, destPath)
}

// newImage runs a qemu-img command that writes a new image at path, which must
// not exist. qemu-img creates files 0644 but writes into an existing one in
// place, so path is created FileMode first: after a chmod, a descriptor another
// account opened in the meantime would go on reading what the guest writes.
func newImage(path string, args ...string) error {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return errQemuImgMissing
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if err = f.Close(); err == nil {
		err = qemuImg(args...)
	}
	if err != nil {
		os.Remove(path) //nolint:errcheck // best-effort cleanup
		return err
	}
	return nil
}

// ResizeDisk grows the disk at path to sizeGB. It never passes --shrink, so a
// shrink that slipped past the caller's check fails in qemu-img instead of
// discarding data. The VM must be off, since QEMU holds the image's write lock.
func ResizeDisk(path string, sizeGB int) error {
	return qemuImg("resize", path, sizeGiB(sizeGB))
}

// InspectDisk reads a disk's virtual size and internal snapshot count.
func InspectDisk(path string) (DiskInfo, error) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return DiskInfo{}, errQemuImgMissing
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("qemu-img", "info", "--output=json", path)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return DiskInfo{}, fmt.Errorf("storage: qemu-img info %q: %w — stderr: %s", path, err, stderr.String())
	}

	var doc struct {
		VirtualSize int64 `json:"virtual-size"`
		Snapshots   []struct {
			ID string `json:"id"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return DiskInfo{}, fmt.Errorf("storage: parse qemu-img info for %q: %w", path, err)
	}
	return DiskInfo{VirtualSizeBytes: doc.VirtualSize, Snapshots: len(doc.Snapshots)}, nil
}

var errQemuImgMissing = fmt.Errorf("storage: qemu-img not found: install with 'apt install qemu-utils' or 'dnf install qemu-img'")

func sizeGiB(gb int) string { return strconv.Itoa(gb) + "G" }

// qemuImg runs one qemu-img subcommand, including its stderr in any error.
func qemuImg(args ...string) error {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return errQemuImgMissing
	}

	var stderr bytes.Buffer
	cmd := exec.Command("qemu-img", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("storage: qemu-img %s failed: %w — stderr: %s", args[0], err, stderr.String())
	}
	return nil
}
