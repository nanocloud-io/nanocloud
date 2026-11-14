package vzvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"github.com/nanocloud-io/nanocloud/internal/storage"
	"github.com/nanocloud-io/nanocloud/internal/vm"
)

// Virtualization.framework has no snapshots, so a snapshot here is an APFS
// clone of each disk and of the EFI variable store. Clones live in
// storage.SnapshotDir beside each disk, so the API's refusal to resize a disk
// that has snapshots applies unchanged. The description, <name>.json, is
// written last: a snapshot that has one is complete.
//
// A running VM is paused while its disks are cloned, giving a crash-consistent
// image. Memory is not saved, so restoring a running snapshot boots the VM.

var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// checkSnapName gates every snapshot entry point, not just create: the name
// is joined into file paths, and a URL path segment can carry an escaped "/".
func checkSnapName(snapName string) error {
	if !snapshotNamePattern.MatchString(snapName) {
		return fmt.Errorf("vzvm: snapshot name %q: use 1–64 letters, digits, dots, dashes or underscores, starting with a letter or digit", snapName)
	}
	return nil
}

const (
	snapStateRunning = "running"
	snapStateShutoff = "shutoff"
)

// snapshotMeta is a snapshot's description, <boot disk>.snapshots/<name>.json.
type snapshotMeta struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// State is "running" or "shutoff", as libvirt reports it.
	State string `json:"state"`
	// Disks are the disks captured, boot disk first.
	Disks []string `json:"disks"`
	// NVRAM is whether the EFI variable store was captured.
	NVRAM bool `json:"nvram,omitempty"`
}

func snapClone(disk, snap string) string {
	return filepath.Join(storage.SnapshotDir(disk), snap+storage.DiskExt)
}
func snapNVRAM(boot, snap string) string {
	return filepath.Join(storage.SnapshotDir(boot), snap+".nvram")
}
func snapMetaPath(boot, snap string) string {
	return filepath.Join(storage.SnapshotDir(boot), snap+".json")
}

// disksOf is every disk of r, boot disk first.
func disksOf(r *record) []string {
	disks := []string{r.disk}
	for _, d := range r.entry.Disks {
		disks = append(disks, d.Path)
	}
	return disks
}

// CreateSnapshot captures the VM's disks and EFI variables under snapName.
func (m *Manager) CreateSnapshot(vmName, snapName, description string) error {
	if err := checkSnapName(snapName); err != nil {
		return err
	}
	r := m.lookup(vmName)
	if r == nil {
		return fmt.Errorf("vzvm: snapshot %q: no such VM", vmName)
	}
	r.op.Lock()
	defer r.op.Unlock()

	m.mu.Lock()
	disks, nvram := disksOf(r), nvramPath(r)
	running, rebooting := r.run != nil, r.run != nil && r.run.ev.Rebooting
	m.mu.Unlock()
	if rebooting {
		return fmt.Errorf("vzvm: snapshot %q: the VM is rebooting; try again in a moment", vmName)
	}
	if _, err := os.Stat(snapMetaPath(disks[0], snapName)); err == nil {
		return fmt.Errorf("vzvm: %q already has a snapshot named %q", vmName, snapName)
	}

	if running {
		if err := simpleCall(m.sockPath(vmName), opPause); err != nil {
			return fmt.Errorf("vzvm: pause %q for a snapshot: %w", vmName, err)
		}
		defer func() {
			if err := simpleCall(m.sockPath(vmName), opResume); err != nil {
				log.Printf("vzvm: resume %q after its snapshot: %v", vmName, err)
			}
		}()
	}

	meta := snapshotMeta{Name: snapName, Description: description, CreatedAt: time.Now().UTC(), State: snapStateShutoff, Disks: disks}
	if running {
		meta.State = snapStateRunning
	}
	var made []string
	undo := func() {
		for _, p := range made {
			_ = os.Remove(p)
		}
	}
	capture := func(src, dst string) error {
		if err := os.MkdirAll(filepath.Dir(dst), storage.DirMode); err != nil {
			return err
		}
		if err := storage.SyncFile(src); err != nil {
			return err
		}
		if err := storage.CloneOrCopy(src, dst); err != nil {
			return err
		}
		made = append(made, dst)
		return nil
	}
	for _, d := range disks {
		if err := capture(d, snapClone(d, snapName)); err != nil {
			undo()
			return fmt.Errorf("vzvm: snapshot %q of %q: %w", snapName, vmName, err)
		}
	}
	if _, err := os.Stat(nvram); err == nil {
		if err := capture(nvram, snapNVRAM(disks[0], snapName)); err != nil {
			undo()
			return fmt.Errorf("vzvm: snapshot %q of %q: %w", snapName, vmName, err)
		}
		meta.NVRAM = true
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err == nil {
		tmp := snapMetaPath(disks[0], snapName) + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, snapMetaPath(disks[0], snapName))
		}
	}
	if err != nil {
		undo()
		return fmt.Errorf("vzvm: snapshot %q of %q: %w", snapName, vmName, err)
	}
	return nil
}

// ListSnapshots lists the VM's snapshots, oldest first.
func (m *Manager) ListSnapshots(vmName string) ([]vm.Snapshot, error) {
	r := m.lookup(vmName)
	if r == nil {
		return nil, fmt.Errorf("vzvm: snapshots of %q: no such VM", vmName)
	}
	m.mu.Lock()
	boot := r.disk
	m.mu.Unlock()

	metas, err := filepath.Glob(filepath.Join(storage.SnapshotDir(boot), "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]vm.Snapshot, 0, len(metas))
	for _, p := range metas {
		meta, err := readSnapshotMeta(p)
		if err != nil {
			log.Printf("vzvm: skipping unreadable snapshot %s: %v", p, err)
			continue
		}
		out = append(out, vm.Snapshot{Name: meta.Name, Description: meta.Description, CreatedAt: meta.CreatedAt, State: meta.State})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// DeleteSnapshot removes one snapshot. The VM is unaffected.
func (m *Manager) DeleteSnapshot(vmName, snapName string) error {
	if err := checkSnapName(snapName); err != nil {
		return err
	}
	r := m.lookup(vmName)
	if r == nil {
		return fmt.Errorf("vzvm: delete snapshot of %q: no such VM", vmName)
	}
	r.op.Lock()
	defer r.op.Unlock()
	m.mu.Lock()
	boot := r.disk
	m.mu.Unlock()

	meta, err := readSnapshotMeta(snapMetaPath(boot, snapName))
	if err != nil {
		return fmt.Errorf("vzvm: %q has no snapshot %q", vmName, snapName)
	}
	// The description goes first, so a half-deleted snapshot is not listed.
	if err := os.Remove(snapMetaPath(boot, snapName)); err != nil {
		return fmt.Errorf("vzvm: delete snapshot %q of %q: %w", snapName, vmName, err)
	}
	for _, d := range meta.Disks {
		_ = os.Remove(snapClone(d, snapName))
	}
	_ = os.Remove(snapNVRAM(boot, snapName))
	return nil
}

// RestoreSnapshot powers the VM off and puts its disks and EFI variables
// back. As with libvirt's revert, a snapshot taken while running is restored
// running. Disks attached or detached since the snapshot are left alone.
func (m *Manager) RestoreSnapshot(vmName, snapName string) error {
	if err := checkSnapName(snapName); err != nil {
		return err
	}
	r := m.lookup(vmName)
	if r == nil {
		return fmt.Errorf("vzvm: restore snapshot of %q: no such VM", vmName)
	}
	r.op.Lock()
	defer r.op.Unlock()
	m.mu.Lock()
	boot, current, nvram := r.disk, disksOf(r), nvramPath(r)
	m.mu.Unlock()

	meta, err := readSnapshotMeta(snapMetaPath(boot, snapName))
	if err != nil {
		return fmt.Errorf("vzvm: %q has no snapshot %q", vmName, snapName)
	}
	attached := make(map[string]bool, len(current))
	for _, d := range current {
		attached[d] = true
	}
	// Check every clone is there before touching anything.
	for _, d := range meta.Disks {
		if !attached[d] {
			continue
		}
		if _, err := os.Stat(snapClone(d, snapName)); err != nil {
			return fmt.Errorf("vzvm: snapshot %q of %q is incomplete: %w", snapName, vmName, err)
		}
	}

	if err := m.Destroy(vmName); err != nil && !errors.Is(err, vm.ErrNotRunning) {
		return fmt.Errorf("vzvm: stop %q to restore it: %w", vmName, err)
	}

	restore := func(src, dst string) error {
		tmp := dst + ".restoring"
		_ = os.Remove(tmp)
		if err := storage.CloneOrCopy(src, tmp); err != nil {
			return err
		}
		return os.Rename(tmp, dst)
	}
	var failed []string
	for _, d := range meta.Disks {
		if !attached[d] {
			continue
		}
		if err := restore(snapClone(d, snapName), d); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", filepath.Base(d), err))
		}
	}
	if meta.NVRAM {
		if err := restore(snapNVRAM(boot, snapName), nvram); err != nil {
			failed = append(failed, fmt.Sprintf("EFI variables: %v", err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("vzvm: restore %q of %q: %s", snapName, vmName, strings.Join(failed, "; "))
	}

	if meta.State == snapStateRunning {
		return m.startLocked(vmName, r)
	}
	m.mu.Lock()
	r.lastReason = libvirt.DomainShutoffFromSnapshot
	m.mu.Unlock()
	return nil
}

// removeSnapshots deletes every snapshot of a VM that is being deleted.
func (m *Manager) removeSnapshots(r *record) {
	for _, d := range disksOf(r) {
		_ = os.RemoveAll(storage.SnapshotDir(d))
	}
}

func readSnapshotMeta(path string) (snapshotMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshotMeta{}, err
	}
	var meta snapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return snapshotMeta{}, err
	}
	if meta.Name == "" || len(meta.Disks) == 0 {
		return snapshotMeta{}, errors.New("missing name or disks")
	}
	return meta, nil
}
