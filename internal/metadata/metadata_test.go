//go:build !windows

package metadata_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nanocloud-io/nanocloud/internal/metadata"
)

func newStore(t *testing.T) *metadata.Store {
	t.Helper()
	return metadata.New(filepath.Join(t.TempDir(), "metadata.json"))
}

func TestWrite_ReadRoundtrip(t *testing.T) {
	s := newStore(t)

	want := map[string]metadata.VMEntry{
		"vm-1": {
			ID:            "vm-1",
			PoolPath:      "/var/lib/nano-cloud/images",
			NetworkMode:   "nat",
			CloudInitUser: "ubuntu",
			Hostname:      "test-host",
		},
	}

	if err := s.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	g, ok := got["vm-1"]
	if !ok {
		t.Fatal("entry vm-1 missing after round-trip")
	}
	if !reflect.DeepEqual(g, want["vm-1"]) {
		t.Errorf("round-trip mismatch:\n  got  %+v\n  want %+v", g, want["vm-1"])
	}
}

func TestDelete_RemovesEntry(t *testing.T) {
	s := newStore(t)

	initial := map[string]metadata.VMEntry{
		"vm-a": {ID: "vm-a", PoolPath: "/pool/a", NetworkMode: "nat"},
		"vm-b": {ID: "vm-b", PoolPath: "/pool/b", NetworkMode: "bridge"},
	}
	if err := s.Write(initial); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := s.Delete("vm-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read after Delete: %v", err)
	}
	if _, exists := got["vm-a"]; exists {
		t.Error("deleted entry vm-a is still present")
	}
	if _, exists := got["vm-b"]; !exists {
		t.Error("entry vm-b was incorrectly removed")
	}
}

// TestUpdate_RefusesUnknownID checks that Update never creates an entry.
func TestUpdate_RefusesUnknownID(t *testing.T) {
	s := newStore(t)

	err := s.Update("ghost", func(e *metadata.VMEntry) { e.Hostname = "nope" })
	if !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("Update on unknown id: got %v, want ErrNotFound", err)
	}

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Update on unknown id created %d entries, want 0: %+v", len(got), got)
	}
}

func TestGet_ReturnsErrNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get("absent"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("Get on unknown id: got %v, want ErrNotFound", err)
	}
}

func TestUpsert_CreatesEntry(t *testing.T) {
	s := newStore(t)

	err := s.Upsert("vm-new", func(e *metadata.VMEntry) {
		e.ID = "vm-new"
		e.PoolPath = "/pool/default"
		e.NetworkMode = "nat"
		e.CloudInitUser = "admin"
		e.Hostname = "my-vm"
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	entry, ok := got["vm-new"]
	if !ok {
		t.Fatal("Upsert did not create entry vm-new")
	}
	if entry.PoolPath != "/pool/default" {
		t.Errorf("PoolPath: got %q, want %q", entry.PoolPath, "/pool/default")
	}
	if entry.NetworkMode != "nat" {
		t.Errorf("NetworkMode: got %q, want %q", entry.NetworkMode, "nat")
	}
	if entry.CloudInitUser != "admin" {
		t.Errorf("CloudInitUser: got %q, want %q", entry.CloudInitUser, "admin")
	}
	if entry.Hostname != "my-vm" {
		t.Errorf("Hostname: got %q, want %q", entry.Hostname, "my-vm")
	}
}

// TestConcurrentWrites_NoCorruption checks that concurrent Upserts of distinct
// IDs all survive.
func TestConcurrentWrites_NoCorruption(t *testing.T) {
	const n = 20
	s := newStore(t)

	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		i := i // capture loop variable
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("vm-%02d", i)
			err := s.Upsert(id, func(e *metadata.VMEntry) {
				e.ID = id
				e.PoolPath = fmt.Sprintf("/pool/%02d", i)
				e.NetworkMode = "nat"
				e.CloudInitUser = fmt.Sprintf("user%02d", i)
				e.Hostname = fmt.Sprintf("host-%02d", i)
			})
			if err != nil {
				t.Errorf("goroutine %d: Upsert failed: %v", i, err)
			}
		}()
	}

	wg.Wait()

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read after concurrent writes: %v", err)
	}

	if len(got) != n {
		t.Fatalf("expected %d entries, got %d", n, len(got))
	}

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("vm-%02d", i)
		entry, ok := got[id]
		if !ok {
			t.Errorf("entry %s missing after concurrent writes", id)
			continue
		}
		wantPool := fmt.Sprintf("/pool/%02d", i)
		if entry.PoolPath != wantPool {
			t.Errorf("entry %s: PoolPath got %q, want %q", id, entry.PoolPath, wantPool)
		}
		wantUser := fmt.Sprintf("user%02d", i)
		if entry.CloudInitUser != wantUser {
			t.Errorf("entry %s: CloudInitUser got %q, want %q", id, entry.CloudInitUser, wantUser)
		}
	}
}

// TestStore_CreatesParentDirectory checks the store creates its parent
// directory rather than failing with ENOENT.
func TestStore_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does", "not", "exist", "metadata.json")
	s := metadata.New(path)

	running := true
	if err := s.Upsert("vm1", func(e *metadata.VMEntry) {
		e.ID = "vm1"
		e.SetRunning(running)
	}); err != nil {
		t.Fatalf("Upsert into a missing directory: %v", err)
	}

	got, err := s.Get("vm1")
	if err != nil {
		t.Fatalf("Get after Upsert: %v", err)
	}
	if r, ok := got.DesiredRunning(); !ok || !r {
		t.Errorf("DesiredRunning() = (%v, %v); want (true, true)", r, ok)
	}
}

// A VM's spec repeats the write_files and runcmd its seed carries.
func TestStore_FileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := metadata.New(path).Upsert("vm1", func(e *metadata.VMEntry) { e.ID = "vm1" }); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("metadata.json has mode %04o, want 0600", got)
	}
}
