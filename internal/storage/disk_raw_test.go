package storage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testdata/tiny-compressed.qcow2 is a 4 MiB, zlib-compressed qcow2 (the
// encoding cloud images ship in) made with:
//
//	truncate -s 4M f.raw
//	printf NANOCLOUD-BOOT-SECTOR | dd of=f.raw conv=notrunc
//	printf NANOCLOUD-3MIB-MARK | dd of=f.raw bs=1 seek=3145728 conv=notrunc
//	qemu-img convert -c -f raw -O qcow2 -o cluster_size=4096 f.raw tiny-compressed.qcow2
const fixtureSize = 4 << 20

func copyFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/tiny-compressed.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "img.qcow2")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readAt(t *testing.T, path string, off int64, n int) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(buf)
}

func TestRawBaseConvertsQcow2Once(t *testing.T) {
	img := copyFixture(t)
	out, err := rawBase(img)
	if err != nil {
		t.Fatal(err)
	}
	if out != RawBasePath(img) || !strings.Contains(out, string(filepath.Separator)+".raw"+string(filepath.Separator)) {
		t.Fatalf("raw base at %s, want the hidden .raw directory beside the image", out)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() != fixtureSize {
		t.Fatalf("raw base size = %v, %v; want %d", st, err, fixtureSize)
	}
	if got := readAt(t, out, 0, 21); got != "NANOCLOUD-BOOT-SECTOR" {
		t.Errorf("offset 0 = %q", got)
	}
	if got := readAt(t, out, 3<<20, 19); got != "NANOCLOUD-3MIB-MARK" {
		t.Errorf("offset 3MiB = %q", got)
	}

	// A second call reuses the conversion.
	before := st.ModTime()
	time.Sleep(10 * time.Millisecond)
	if again, err := rawBase(img); err != nil || again != out {
		t.Fatalf("second rawBase = %s, %v", again, err)
	}
	if st2, _ := os.Stat(out); !st2.ModTime().Equal(before) {
		t.Error("the image was converted again although it had not changed")
	}

	// A re-pulled (newer) image is converted afresh.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(img, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := rawBase(img); err != nil {
		t.Fatal(err)
	}
	if st3, _ := os.Stat(out); st3.ModTime().Equal(before) {
		t.Error("a newer image was not reconverted")
	}
}

func TestRawBaseUsesARawImageAsIs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "img.raw")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := rawBase(p); err != nil || out != p {
		t.Fatalf("rawBase(raw) = %s, %v; want the image itself", out, err)
	}
}

func TestRawBaseConcurrent(t *testing.T) {
	img := copyFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := rawBase(img)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAllocateRaw(t *testing.T) {
	img := copyFixture(t)
	dir := t.TempDir()
	disk := filepath.Join(dir, "web.raw")
	if err := allocateRaw(img, disk, 1); err != nil {
		t.Fatal(err)
	}
	// Cloned from a 0644 base, so this is the mode the copy was given.
	assertMode(t, disk, FileMode)
	if left, _ := filepath.Glob(filepath.Join(dir, ".clone-*")); len(left) > 0 {
		t.Errorf("staging left behind: %v", left)
	}
	info, err := inspectRaw(disk)
	if err != nil || info.VirtualSizeBytes != BytesPerGB || info.Snapshots != 0 {
		t.Fatalf("inspect = %+v, %v; want 1 GiB", info, err)
	}
	if got := readAt(t, disk, 3<<20, 19); got != "NANOCLOUD-3MIB-MARK" {
		t.Errorf("disk does not hold the image: %q", got)
	}
	if err := allocateRaw(img, disk, 1); err == nil {
		t.Error("allocating over an existing disk succeeded")
	}
}

func TestAllocateRawTooSmall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.raw")
	if err := os.WriteFile(p, []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 3*BytesPerGB); err != nil {
		t.Fatal(err)
	}
	err := allocateRaw(p, filepath.Join(t.TempDir(), "d.raw"), 2)
	if err == nil || !strings.Contains(err.Error(), "needs 3 GiB") {
		t.Fatalf("allocate 2 GiB from a 3 GiB image: %v", err)
	}
}

func TestCreateResizeCopyRaw(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "web-vdb.raw")
	if err := createRaw(disk, 1); err != nil {
		t.Fatal(err)
	}
	assertMode(t, disk, FileMode)
	if err := createRaw(disk, 1); err == nil {
		t.Error("createRaw overwrote an existing disk")
	}
	if err := resizeRaw(disk, 2); err != nil {
		t.Fatal(err)
	}
	if info, _ := inspectRaw(disk); info.VirtualSizeBytes != 2*BytesPerGB {
		t.Errorf("after resize: %d bytes", info.VirtualSizeBytes)
	}
	if err := resizeRaw(disk, 1); err == nil {
		t.Error("resizeRaw shrank a disk")
	}

	// The copy path, on a small sparse file: the clone fallback reads every byte.
	small := filepath.Join(dir, "small.raw")
	if err := os.WriteFile(small, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(small, 8<<20); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(small, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("DATA"), 5<<20+7); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(dir, "clone.raw")
	if err := copyRaw(small, cp); err != nil {
		t.Fatal(err)
	}
	if info, _ := inspectRaw(cp); info.VirtualSizeBytes != 8<<20 {
		t.Errorf("copy is %d bytes", info.VirtualSizeBytes)
	}
	if got := readAt(t, cp, 5<<20+7, 4); got != "DATA" {
		t.Errorf("copy lost data: %q", got)
	}
	assertMode(t, cp, FileMode)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s has mode %04o, want %04o", filepath.Base(path), got, want)
	}
}

func TestInspectRawCountsSnapshots(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "web"+DiskExt)
	if err := createRaw(disk, 1); err != nil {
		t.Fatal(err)
	}
	if info, _ := inspectRaw(disk); info.Snapshots != 0 {
		t.Fatalf("fresh disk has %d snapshots", info.Snapshots)
	}
	dir := SnapshotDir(disk)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a" + DiskExt, "b" + DiskExt, "a.json", "a.nvram"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if info, _ := inspectRaw(disk); info.Snapshots != 2 {
		t.Fatalf("Snapshots = %d, want 2 (one per disk clone, not per file)", info.Snapshots)
	}
}

func TestCloneOrCopyRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := CloneOrCopy(src, dst); err == nil {
		t.Fatal("CloneOrCopy overwrote an existing file")
	}
	if got, _ := os.ReadFile(dst); string(got) != dst {
		t.Fatal("the existing file was changed")
	}
}
