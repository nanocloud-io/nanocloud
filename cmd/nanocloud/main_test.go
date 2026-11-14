package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/storage"
)

// A VM created by an older release has 0644 files. They are tightened on
// start, unless QEMU could only read them through the "other" bits.
func TestRestrictVMFiles(t *testing.T) {
	pool := t.TempDir()
	disk, iso, _ := storage.PathsFor(pool, "web")
	data := filepath.Join(pool, "web-vdb"+storage.DiskExt)
	snap := filepath.Join(storage.SnapshotDir(disk), "before"+storage.DiskExt)
	unrelated := filepath.Join(pool, "notes.txt")
	if err := os.Mkdir(storage.SnapshotDir(disk), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{disk, iso, data, snap, unrelated}
	for _, p := range files {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	chmodAll := func() {
		for _, p := range files {
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	modes := func() map[string]os.FileMode {
		m := make(map[string]os.FileMode)
		for _, p := range files {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			m[filepath.Base(p)] = st.Mode().Perm()
		}
		return m
	}

	store := metadata.New(filepath.Join(t.TempDir(), "metadata.json"))
	if err := store.Upsert("web", func(e *metadata.VMEntry) {
		e.ID, e.PoolPath = "web", pool
		e.Disks = []metadata.Disk{{Target: "vdb", Path: data, SizeGB: 1}}
	}); err != nil {
		t.Fatal(err)
	}

	chmodAll()
	restrictVMFiles(store, false)
	for name, mode := range modes() {
		if mode != 0o644 {
			t.Errorf("with QEMU unable to open private files, %s became %04o", name, mode)
		}
	}

	restrictVMFiles(store, true)
	for name, mode := range modes() {
		want := os.FileMode(0o600)
		if name == filepath.Base(unrelated) {
			want = 0o644
		}
		if mode != want {
			t.Errorf("%s has mode %04o, want %04o", name, mode, want)
		}
	}
}
