package vzvm

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"github.com/nanocloud-io/nanocloud/internal/hypervisor"
	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/network"
	"github.com/nanocloud-io/nanocloud/internal/storage"
	"github.com/nanocloud-io/nanocloud/internal/vm"
	"github.com/nanocloud-io/nanocloud/internal/vswitch"
)

// This backend reports machine state in libvirt's vocabulary so the reconcile
// loop runs unchanged:
//
//	never started, or stopped         DomainShutoff + the reason it stopped
//	guest powered off / Stop()        DomainShutoffShutdown  (honoured as intent)
//	Destroy()                         DomainShutoffDestroyed
//	framework error state             DomainShutoffCrashed   (restarted under on-failure)
//	the Mac shut down under a VM      DomainShutoffUnknown   (restarted under on-failure)
//	starting / resuming / rebooting   DomainNostate          (reconcile waits)
//	running                           DomainRunning
//	pausing / paused / saving         DomainPaused
//	stopping                          DomainShutdown         (reconcile waits)
//
// A guest reboot never shows up here: the helper boots the VM straight back up.

// stopTimeout bounds how long Destroy waits for a machine to report Stopped.
const stopTimeout = 30 * time.Second

// Config configures a Manager.
type Config struct {
	// Spawn starts a helper process for a VM.
	Spawn Spawner
	// RunDir holds each running VM's socket and each stopped VM's status
	// file. Unix socket paths are limited to about 100 bytes, so keep it short.
	RunDir string
	// Leases finds NAT guests' addresses when the guest does not report its own.
	Leases network.LeaseSource
	// Switch is the control socket of the switch linking every VM's NIC into
	// one network. Empty leaves each VM on Apple's NAT, which cannot reach
	// other VMs.
	Switch string
	// EnsureSwitch starts the switch unless it is running. It is called
	// before every boot, and every switchCheckInterval while any VM runs.
	EnsureSwitch func() error
}

// Manager is the Virtualization.framework backend, as seen from the daemon.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	vms     map[string]*record
	nextID  int32
	closing bool
	stop    chan struct{} // closed by Close
}

// record is one VM this backend knows how to build.
type record struct {
	entry     metadata.VMEntry
	disk, iso string
	mac       net.HardwareAddr
	uuid      string
	// op serialises the operations that must not interleave on one VM:
	// starting it, deleting it, and taking or restoring a snapshot.
	op sync.Mutex
	// run is the helper running this VM, nil while it is stopped.
	run *run
	// starting is set while Start waits for a helper to boot the VM.
	starting bool
	// lastReason is why the VM last stopped.
	lastReason libvirt.DomainShutoffReason
	// probe tracks whether the lease address answers SSH during this boot.
	probe sshProbe
}

// sshProbe is the outcome of probing one address on port 22 during one boot.
type sshProbe struct {
	startedAt int64
	ip        string
	ok        bool
	at        time.Time
	inflight  bool
}

// run is the daemon's view of one helper.
type run struct {
	id     int32
	active metadata.VMEntry
	ev     event
	watch  net.Conn
	done   chan struct{}
}

// New creates a Manager.
func New(cfg Config) *Manager {
	if cfg.RunDir != "" {
		if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
			log.Printf("vzvm: %v", err)
		}
	}
	m := &Manager{cfg: cfg, vms: make(map[string]*record), stop: make(chan struct{})}
	if cfg.EnsureSwitch != nil {
		go m.superviseSwitch()
	}
	return m
}

// switchCheckInterval is how often the switch is checked while VMs run.
var switchCheckInterval = 5 * time.Second

// superviseSwitch restarts the switch if it dies while VMs depend on it.
// Helpers re-attach on their own, so guests lose packets briefly but keep
// their links.
func (m *Manager) superviseSwitch() {
	t := time.NewTicker(switchCheckInterval)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		if !m.anyRunning() {
			continue
		}
		msg := ""
		if err := m.cfg.EnsureSwitch(); err != nil {
			msg = err.Error()
		}
		switch {
		case msg != "" && msg != last:
			log.Printf("vzvm: network switch: %s", msg)
		case msg == "" && last != "":
			log.Printf("vzvm: network switch is back")
		}
		last = msg
	}
}

func (m *Manager) anyRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.vms {
		if r.run != nil {
			return true
		}
	}
	return false
}

// SwitchStatus reports the switch every VM's NIC is linked into, or
// vswitch.ErrNoSwitch when VMs are on Apple's NAT instead.
func (m *Manager) SwitchStatus() (vswitch.Report, error) {
	if m.cfg.Switch == "" {
		return vswitch.Report{}, vswitch.ErrNoSwitch
	}
	return vswitch.Ping(m.cfg.Switch)
}

var _ interface {
	hypervisor.Backend
	hypervisor.Capable
	hypervisor.SerialConsoler
	hypervisor.Snapshotter
} = (*Manager)(nil)

// Capabilities reports what this backend offers. It has no VNC, passthrough,
// firmware choice (always UEFI), TPM, CPU shaping, per-VM usage counters,
// bridged networking or VLANs.
func (m *Manager) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		Backend:       hypervisor.BackendVZ,
		Snapshots:     true,
		SerialConsole: true,
		HostStats:     true,
	}
}

// Connect is a no-op: there is no daemon to reach.
func (m *Manager) Connect() error { return nil }

// Close lets go of the helpers without stopping them: the VMs keep running
// while the daemon restarts or is upgraded, and the next daemon picks them
// back up in AdoptSpecs.
func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closing {
		close(m.stop)
	}
	m.closing = true
	var conns []net.Conn
	for _, r := range m.vms {
		if r.run != nil {
			conns = append(conns, r.run.watch)
		}
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.Close() //nolint:errcheck // letting go
	}
	return nil
}

// HostCPUs is the number of host CPUs a guest can be given.
func (m *Manager) HostCPUs() int { return runtime.NumCPU() }

// AdoptSpecs registers every VM in entries and re-attaches to those whose
// helper is still running. A VM whose helper is gone is reported stopped with
// the reason the helper recorded. Entries without a pool are skipped.
func (m *Manager) AdoptSpecs(entries map[string]metadata.VMEntry) {
	for id, e := range entries {
		if e.ID == "" {
			e.ID = id
		}
		disk, iso, ok := storage.PathsFor(e.PoolPath, e.ID)
		if !ok {
			continue
		}
		if err := m.DefineFromSpec(e, disk, iso); err != nil {
			log.Printf("vzvm: cannot adopt %q: %v", e.ID, err)
			continue
		}
		if m.attach(e.ID, m.lookup(e.ID)) {
			log.Printf("vzvm: %q is still running; picked it back up", e.ID)
		}
	}
}

// Create defines a VM and boots it. A VM that defines but will not boot is
// removed again, so a failed create leaves nothing behind.
func (m *Manager) Create(p vm.VMCreateParams) error {
	e := entryOf(p)
	if err := m.DefineFromSpec(e, p.DiskPath, p.CloudInitISO); err != nil {
		return err
	}
	if err := m.Start(p.Name); err != nil {
		_ = m.Delete(p.Name)
		return err
	}
	return nil
}

// DefineFromSpec registers or updates the VM e describes. A running VM keeps
// its current machine until its next boot.
func (m *Manager) DefineFromSpec(e metadata.VMEntry, diskPath, isoPath string) error {
	if err := validate(e); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.vms[e.ID]
	if !ok {
		r = &record{lastReason: libvirt.DomainShutoffUnknown}
		m.vms[e.ID] = r
	}
	if r.mac == nil {
		mac, err := macOf(e.MAC)
		if err != nil {
			return fmt.Errorf("vzvm: define %q: %w", e.ID, err)
		}
		r.mac = mac
	}
	if r.uuid == "" {
		r.uuid = e.UUID
		if r.uuid == "" {
			r.uuid = newUUID()
		}
	}
	r.entry, r.disk, r.iso = e, diskPath, isoPath
	return nil
}

// RedefineFromSpec is DefineFromSpec, refusing to change an identity the guest
// has already seen: a new MAC would cost the VM its DHCP lease and address.
func (m *Manager) RedefineFromSpec(e metadata.VMEntry, diskPath, isoPath string) error {
	m.mu.Lock()
	r, ok := m.vms[e.ID]
	if ok && e.MAC != "" && network.NormalizeMAC(e.MAC) != r.mac.String() {
		m.mu.Unlock()
		return fmt.Errorf("vzvm: redefine %q would change its MAC from %s to %s", e.ID, r.mac, e.MAC)
	}
	m.mu.Unlock()
	return m.DefineFromSpec(e, diskPath, isoPath)
}

// validate refuses what this backend cannot build, before anything is started.
func validate(e metadata.VMEntry) error {
	if e.ID == "" || e.VCPUs <= 0 || e.MemoryMB <= 0 {
		return fmt.Errorf("vzvm: cannot build %q: %w (vcpus=%d memory_mb=%d)", e.ID, vm.ErrIncompleteSpec, e.VCPUs, e.MemoryMB)
	}
	var missing []string
	if len(e.PCIDevices)+len(e.USBDevices) > 0 {
		missing = append(missing, "PCI/USB passthrough")
	}
	if e.CPUSet != "" || e.CPUMode != "" || e.CPUTopology != nil {
		missing = append(missing, "CPU model, topology or pinning")
	}
	if e.TPM {
		missing = append(missing, "a TPM")
	}
	if e.SecureBoot {
		missing = append(missing, "secure boot")
	}
	if e.NetworkMode == metadata.NetworkBridge {
		missing = append(missing, "bridged networking")
	}
	if len(missing) > 0 {
		return fmt.Errorf("vzvm: %q asks for %v: %w", e.ID, missing, hypervisor.ErrUnsupported)
	}
	return nil
}

// Delete powers off and forgets a VM, removing the files only this backend
// writes: EFI variables, machine identifier, status file and snapshots. The
// disk and seed belong to the caller. An unknown VM is not an error.
func (m *Manager) Delete(name string) error {
	r := m.lookup(name)
	if r == nil {
		return nil
	}
	r.op.Lock()
	defer r.op.Unlock()

	if err := m.Destroy(name); err != nil && !errors.Is(err, vm.ErrNotRunning) {
		return fmt.Errorf("vzvm: delete %q: %w", name, err)
	}
	m.mu.Lock()
	if r.run != nil || r.starting {
		m.mu.Unlock()
		return fmt.Errorf("vzvm: delete %q: it is still running", name)
	}
	delete(m.vms, name)
	m.mu.Unlock()

	m.removeSnapshots(r)
	for _, p := range []string{nvramPath(r), machineIDPath(r), m.statusPath(name)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("vzvm: delete %q: remove %s: %v", name, p, err)
		}
	}
	return nil
}

// Start boots a stopped VM in a new helper process.
func (m *Manager) Start(name string) error {
	r := m.lookup(name)
	if r == nil {
		return fmt.Errorf("vzvm: start %q: no such VM", name)
	}
	r.op.Lock()
	defer r.op.Unlock()
	return m.startLocked(name, r)
}

// startLocked is Start with r.op already held.
func (m *Manager) startLocked(name string, r *record) error {
	m.mu.Lock()
	if r.run != nil || r.starting {
		m.mu.Unlock()
		return fmt.Errorf("vzvm: start %q: already running", name)
	}
	if m.cfg.Spawn == nil {
		m.mu.Unlock()
		return fmt.Errorf("vzvm: start %q: no helper spawner configured", name)
	}
	r.starting = true
	spec := m.helperSpec(name, r)
	disk := r.disk
	m.mu.Unlock()

	err := func() error {
		if _, err := os.Stat(disk); err != nil {
			return fmt.Errorf("boot disk: %w", err)
		}
		if m.cfg.EnsureSwitch != nil {
			if err := m.cfg.EnsureSwitch(); err != nil {
				return fmt.Errorf("network switch: %w", err)
			}
		}
		_ = os.Remove(m.statusPath(name))
		return m.cfg.Spawn(spec)
	}()

	m.mu.Lock()
	r.starting = false
	if err != nil {
		r.lastReason = libvirt.DomainShutoffFailed
	}
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("vzvm: start %q: %w", name, err)
	}
	// A guest that powered straight off is not a start failure; attach
	// records why it stopped.
	m.attach(name, r)
	return nil
}

// helperSpec describes the machine for r's next boot. m.mu must be held.
func (m *Manager) helperSpec(name string, r *record) HelperSpec {
	disks := []Disk{{Path: r.disk}}
	for _, d := range r.entry.Disks {
		disks = append(disks, Disk{Path: d.Path})
	}
	if r.iso != "" {
		if _, err := os.Stat(r.iso); err == nil {
			disks = append(disks, Disk{Path: r.iso, ReadOnly: true})
		}
	}
	return HelperSpec{
		Machine: MachineConfig{
			Name:          name,
			CPUs:          uint(r.entry.VCPUs),
			MemoryBytes:   uint64(r.entry.MemoryMB) << 20,
			Disks:         disks,
			MAC:           r.mac.String(),
			NVRAMPath:     nvramPath(r),
			MachineIDPath: machineIDPath(r),
		},
		Active: r.entry,
		Socket: m.sockPath(name),
		Status: m.statusPath(name),
		Switch: m.cfg.Switch,
	}
}

// attach connects to the helper running name, if there is one, and follows
// it. Without one, it records why the VM last stopped, from the status file
// its helper left. It reports whether the VM is running.
func (m *Manager) attach(name string, r *record) bool {
	gone := func() bool {
		if st, ok := readStatus(m.statusPath(name)); ok {
			m.mu.Lock()
			r.lastReason = reasonCode(st.Reason)
			m.mu.Unlock()
		}
		return false
	}

	var inf helperInfo
	if err := callHelper(m.sockPath(name), opInfo, &inf); err != nil || !inf.OK {
		return gone()
	}
	c, br, err := openHelper(m.sockPath(name), opWatch)
	if err != nil {
		return gone()
	}
	line, err := br.ReadBytes('\n')
	var first event
	if err != nil || json.Unmarshal(line, &first) != nil || first.Final {
		c.Close() //nolint:errcheck // nothing to follow
		return gone()
	}

	rn := &run{active: inf.Active, ev: first, watch: c, done: make(chan struct{})}
	m.mu.Lock()
	m.nextID++
	rn.id = m.nextID
	r.run = rn
	m.mu.Unlock()
	go m.follow(name, r, rn, br)
	return true
}

// follow applies a helper's events until its VM stops for good.
func (m *Manager) follow(name string, r *record, rn *run, br interface{ ReadBytes(byte) ([]byte, error) }) {
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break
		}
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		m.mu.Lock()
		if !ev.Final && ev.StartedAt == 0 {
			ev.StartedAt = rn.ev.StartedAt
		}
		rn.ev = ev
		if ev.Final {
			r.lastReason = reasonCode(ev.Reason)
			if r.run == rn {
				r.run = nil
			}
		}
		m.mu.Unlock()
		if ev.Final {
			rn.watch.Close() //nolint:errcheck // done
			close(rn.done)
			return
		}
	}

	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		return // the daemon is exiting; the VM carries on
	}
	// No final event: trust the status file if there is one, else it crashed.
	reason := reasonCrashed
	if st, ok := readStatus(m.statusPath(name)); ok {
		reason = st.Reason
	}
	log.Printf("vzvm: lost the helper for %q (%s)", name, reason)
	m.mu.Lock()
	r.lastReason = reasonCode(reason)
	if r.run == rn {
		r.run = nil
	}
	m.mu.Unlock()
	close(rn.done)
}

// reasonCode maps a helper's stop reason onto libvirt's.
func reasonCode(reason string) libvirt.DomainShutoffReason {
	switch reason {
	case reasonShutdown:
		return libvirt.DomainShutoffShutdown
	case reasonDestroyed:
		return libvirt.DomainShutoffDestroyed
	case reasonCrashed:
		return libvirt.DomainShutoffCrashed
	case reasonFailed:
		return libvirt.DomainShutoffFailed
	default: // reasonHost, or unknown
		return libvirt.DomainShutoffUnknown
	}
}

// Stop asks the guest to shut down and returns without waiting, like an ACPI
// power-button press on libvirt.
func (m *Manager) Stop(name string) error {
	if _, err := m.running(name, "shutdown"); err != nil {
		return err
	}
	if err := simpleCall(m.sockPath(name), opStop); err != nil {
		return fmt.Errorf("vzvm: shutdown %q: %w", name, err)
	}
	return nil
}

// Destroy powers a VM off immediately and waits until it has stopped, so the
// caller can delete its disk straight after.
func (m *Manager) Destroy(name string) error {
	rn, err := m.running(name, "destroy")
	if err != nil {
		return err
	}
	if err := simpleCall(m.sockPath(name), opDestroy); err != nil {
		select {
		case <-rn.done: // it stopped on its own meanwhile
			return nil
		default:
		}
		return fmt.Errorf("vzvm: destroy %q: %w", name, err)
	}
	select {
	case <-rn.done:
		return nil
	case <-time.After(stopTimeout):
		return fmt.Errorf("vzvm: destroy %q: still not stopped after %s", name, stopTimeout)
	}
}

// running returns name's helper, or vm.ErrNotRunning.
func (m *Manager) running(name, action string) (*run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.vms[name]
	if !ok {
		return nil, fmt.Errorf("vzvm: %s %q: no such VM", action, name)
	}
	if r.run == nil {
		return nil, fmt.Errorf("vzvm: %s %q: %w", action, name, vm.ErrNotRunning)
	}
	return r.run, nil
}

// Observe reports every VM this backend knows, in libvirt's vocabulary.
func (m *Manager) Observe() ([]vm.ObservedDomain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vm.ObservedDomain, 0, len(m.vms))
	for name, r := range m.vms {
		state, reason := r.observeLocked()
		out = append(out, vm.ObservedDomain{Name: name, State: state, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *record) observeLocked() (libvirt.DomainState, int32) {
	if r.starting {
		return libvirt.DomainNostate, 0
	}
	if r.run == nil {
		return libvirt.DomainShutoff, int32(r.lastReason)
	}
	if r.run.ev.Rebooting {
		return libvirt.DomainNostate, 0
	}
	switch r.run.ev.State {
	case MachineRunning:
		return libvirt.DomainRunning, 0
	case MachinePaused, MachinePausing, MachineSaving:
		return libvirt.DomainPaused, 0
	case MachineStopping:
		return libvirt.DomainShutdown, 0
	default:
		// In flux (starting, resuming, restoring, stopping); reconcile waits.
		return libvirt.DomainNostate, 0
	}
}

func stateString(s libvirt.DomainState) string {
	switch s {
	case libvirt.DomainRunning:
		return "running"
	case libvirt.DomainPaused:
		return "paused"
	case libvirt.DomainShutdown, libvirt.DomainShutoff:
		return "stopped"
	default:
		return "unknown"
	}
}

func infoLocked(name string, r *record) vm.VM {
	state, _ := r.observeLocked()
	v := vm.VM{ID: -1, Name: name, State: stateString(state), VCPUs: uint16(r.entry.VCPUs)}
	if r.run != nil {
		v.ID = r.run.id
		// The framework reports no in-guest usage, so this is the allocation.
		v.MemoryKB = uint64(r.run.active.MemoryMB) * 1024
	}
	return v
}

// List reports every VM this backend knows.
func (m *Manager) List() ([]vm.VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vm.VM, 0, len(m.vms))
	for name, r := range m.vms {
		out = append(out, infoLocked(name, r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get reports one VM.
func (m *Manager) Get(name string) (*vm.VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.vms[name]
	if !ok {
		return nil, fmt.Errorf("vzvm: lookup %q: no such VM", name)
	}
	v := infoLocked(name, r)
	return &v, nil
}

// Exists reports whether this backend knows the VM.
func (m *Manager) Exists(name string) (bool, error) {
	return m.lookup(name) != nil, nil
}

// Identity is the MAC and UUID this backend gave the VM, for the reconcile
// loop to record in the spec so they survive a daemon restart.
func (m *Manager) Identity(name string) (vm.DomainIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.vms[name]
	if !ok {
		return vm.DomainIdentity{}, fmt.Errorf("vzvm: lookup %q to read its identity: no such VM", name)
	}
	return vm.DomainIdentity{UUID: r.uuid, MAC: r.mac.String()}, nil
}

// ActiveHardware returns the running boot's definition while the VM runs, and
// the stored one once it stops; "restart to apply" compares against it.
func (m *Manager) ActiveHardware(name string) (vm.ActiveHardware, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.vms[name]
	if !ok {
		return vm.ActiveHardware{}, fmt.Errorf("vzvm: lookup %q: no such VM", name)
	}
	e := r.entry
	if r.run != nil {
		e = r.run.active
	}
	return vm.ActiveHardware{
		HostDevs:    vm.HostDevRefs(e),
		CPU:         vm.CPUShapeOf(e),
		Network:     vm.NetworkShapeOf(e),
		DiskTargets: vm.DiskTargets(e),
	}, nil
}

// SampleAll reports each VM's configured size; the framework exposes no CPU
// time or in-guest memory counters.
func (m *Manager) SampleAll() ([]vm.VMSample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]vm.VMSample, 0, len(m.vms))
	for name, r := range m.vms {
		state, _ := r.observeLocked()
		out = append(out, vm.VMSample{
			Name:     name,
			State:    state,
			MaxMemKB: uint64(r.entry.MemoryMB) * 1024,
			NrVCPU:   uint16(r.entry.VCPUs),
			At:       now,
		})
	}
	return out, nil
}

// PrimaryIP returns a running VM's address. It is confirmed when the guest
// reported it on its console, or when the leased address answers on port 22;
// a lease alone confirms nothing because it outlives the guest that held it.
func (m *Manager) PrimaryIP(name string) (ip string, confirmed bool) {
	m.mu.Lock()
	r, ok := m.vms[name]
	if !ok || r.run == nil {
		m.mu.Unlock()
		return "", false
	}
	ev, mac := r.run.ev, r.mac.String()
	m.mu.Unlock()

	if ev.GuestIP != "" {
		return ev.GuestIP, true
	}
	if m.cfg.Leases == nil {
		return "", false
	}
	leases, err := m.cfg.Leases.Read()
	if err != nil {
		return "", false
	}
	for _, l := range leases {
		if network.NormalizeMAC(l.MACAddress) == mac {
			return l.IPAddress, m.answersSSH(r, ev.StartedAt, l.IPAddress)
		}
	}
	return "", false
}

// sshProbeInterval is how soon an address that did not answer is probed again.
var sshProbeInterval = 5 * time.Second

// probeSSH reports whether ip accepts connections on port 22. A variable so
// tests need no network.
var probeSSH = func(ip string) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close() //nolint:errcheck // only the connect mattered
	return true
}

// answersSSH reports whether ip is known to have answered on port 22 this
// boot, probing in the background when unknown. It never blocks, since GET
// /vms calls it for every VM.
func (m *Manager) answersSSH(r *record, startedAt int64, ip string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.probe.startedAt != startedAt || r.probe.ip != ip {
		r.probe = sshProbe{startedAt: startedAt, ip: ip}
	}
	if r.probe.ok {
		return true
	}
	if !r.probe.inflight && time.Since(r.probe.at) >= sshProbeInterval {
		r.probe.inflight = true
		go func() {
			ok := probeSSH(ip)
			m.mu.Lock()
			defer m.mu.Unlock()
			if r.probe.startedAt == startedAt && r.probe.ip == ip {
				r.probe.ok, r.probe.at, r.probe.inflight = ok, time.Now(), false
			}
		}()
	}
	return false
}

// OpenSerial attaches to a running VM's console. Reads replay recent output,
// then follow it live; writes are typed into the guest.
func (m *Manager) OpenSerial(name string) (io.ReadWriteCloser, error) {
	if _, err := m.running(name, "open the console of"); err != nil {
		return nil, err
	}
	c, br, err := openHelper(m.sockPath(name), opSerial)
	if err != nil {
		return nil, fmt.Errorf("vzvm: console of %q: %w", name, err)
	}
	line, err := br.ReadBytes('\n')
	var r reply
	if err == nil {
		err = json.Unmarshal(line, &r)
	}
	if err == nil {
		err = r.err()
	}
	if err != nil {
		c.Close() //nolint:errcheck // already failing
		return nil, fmt.Errorf("vzvm: console of %q: %w", name, err)
	}
	return bufferedConn{Conn: c, r: br}, nil
}

func (m *Manager) lookup(name string) *record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vms[name]
}

func (m *Manager) sockPath(name string) string   { return filepath.Join(m.cfg.RunDir, name+".sock") }
func (m *Manager) statusPath(name string) string { return filepath.Join(m.cfg.RunDir, name+".status") }

func entryOf(p vm.VMCreateParams) metadata.VMEntry {
	return metadata.VMEntry{
		ID:          p.Name,
		NetworkMode: p.NetworkMode,
		Bridge:      p.Bridge,
		VLANID:      p.VLANID,
		VLANUplink:  p.VLANUplink,
		VCPUs:       p.VCPUs,
		MemoryMB:    p.MemoryMB,
		Firmware:    p.Firmware,
		SecureBoot:  p.SecureBoot,
		TPM:         p.TPM,
		CPUMode:     p.CPUMode,
		CPUTopology: p.CPUTopology,
		CPUSet:      p.CPUSet,
		PCIDevices:  p.PCIDevices,
		USBDevices:  p.USBDevices,
	}
}

func nvramPath(r *record) string { return filepath.Join(filepath.Dir(r.disk), r.entry.ID+".nvram") }
func machineIDPath(r *record) string {
	return filepath.Join(filepath.Dir(r.disk), r.entry.ID+".machineid")
}

// macOf parses a recorded MAC, or makes a random locally administered unicast
// one when none is recorded yet.
func macOf(s string) (net.HardwareAddr, error) {
	if s != "" {
		mac, err := net.ParseMAC(network.NormalizeMAC(s))
		if err != nil {
			return nil, fmt.Errorf("bad MAC %q: %w", s, err)
		}
		return mac, nil
	}
	mac := make(net.HardwareAddr, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, err
	}
	mac[0] = (mac[0] | 0x02) &^ 0x01
	return mac, nil
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
