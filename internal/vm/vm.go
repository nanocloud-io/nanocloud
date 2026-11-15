// Package vm manages virtual machine lifecycle via libvirt.
package vm

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"

	"github.com/nanocloud-io/nanocloud/internal/metadata"
)

// VM holds the fields returned by List for a single domain.
type VM struct {
	// ID is the libvirt runtime domain ID (-1 for inactive domains).
	ID int32 `json:"id"`
	// Name is the domain name as registered in libvirt.
	Name string `json:"name"`
	// State is "running", "paused", "stopped" or "unknown".
	State string `json:"state"`
	// VCPUs is the number of virtual CPUs configured for the domain.
	VCPUs uint16 `json:"vcpus"`
	// MemoryKB is the memory currently used by the domain, in kibibytes.
	MemoryKB uint64 `json:"memory_kb"`
}

// VMSample is one raw counter reading for a domain, taken by SampleAll.
// MemUsedKB is in-guest usage, not the balloon allocation; see usedMemoryKB.
type VMSample struct {
	Name      string
	State     libvirt.DomainState
	CPUTimeNs uint64
	MemUsedKB uint64
	MaxMemKB  uint64
	NrVCPU    uint16
	At        time.Time
}

// SampleAll returns one raw counter snapshot per domain, active and inactive.
// A domain whose info cannot be read is skipped.
func (m *Manager) SampleAll() ([]VMSample, error) {
	lv, err := m.conn()
	if err != nil {
		return nil, err
	}

	flags := libvirt.ConnectListDomainsActive | libvirt.ConnectListDomainsInactive
	domains, _, err := lv.ConnectListAllDomains(1, flags)
	if err != nil {
		return nil, fmt.Errorf("vm: sample all domains: %w", err)
	}

	now := time.Now()
	out := make([]VMSample, 0, len(domains))
	for _, d := range domains {
		state, maxMem, mem, nrVCPU, cpuTime, err := lv.DomainGetInfo(d)
		if err != nil {
			continue
		}
		// DomainGetInfo's mem is the balloon allocation, which for an un-ballooned
		// guest is its configured size and would always read as 100% used.
		usedKB := mem
		if libvirt.DomainState(state) == libvirt.DomainRunning {
			usedKB = m.usedMemoryKB(lv, d, mem)
		}
		out = append(out, VMSample{
			Name: d.Name, State: libvirt.DomainState(state), CPUTimeNs: cpuTime,
			MemUsedKB: usedKB, MaxMemKB: maxMem, NrVCPU: nrVCPU, At: now,
		})
	}
	return out, nil
}

// memStatsPeriodSeconds is the virtio-balloon stats collection interval.
const memStatsPeriodSeconds = 1

// usedMemoryKB returns the guest's used memory in KiB: balloon stats
// (available - unused) when reported, then host RSS, then fallbackKB.
func (m *Manager) usedMemoryKB(lv *libvirt.Libvirt, d libvirt.Domain, fallbackKB uint64) uint64 {
	// Best-effort: without a stats period the balloon reports no available/unused.
	_ = lv.DomainSetMemoryStatsPeriod(d, memStatsPeriodSeconds, 0)

	stats, err := lv.DomainMemoryStats(d, uint32(libvirt.DomainMemoryStatNr), 0)
	if err != nil {
		return fallbackKB
	}

	var available, unused, rss uint64
	var haveAvail, haveUnused bool
	for _, s := range stats {
		switch libvirt.DomainMemoryStatTags(s.Tag) {
		case libvirt.DomainMemoryStatAvailable:
			available, haveAvail = s.Val, true
		case libvirt.DomainMemoryStatUnused:
			unused, haveUnused = s.Val, true
		case libvirt.DomainMemoryStatRss:
			rss = s.Val
		}
	}

	if haveAvail && haveUnused && available >= unused {
		return available - unused
	}
	if rss > 0 {
		return rss
	}
	return fallbackKB
}

// hostCaps are the host's QEMU/KVM capabilities, read from libvirt at connect
// time so the domain XML need not hardcode a machine type or emulator path.
type hostCaps struct {
	// Emulator is the absolute path to the QEMU binary on the host.
	Emulator string
	// Machine is the machine type to use (e.g. "q35", "virt", "pc").
	Machine string
	// Arch is the guest CPU architecture (e.g. "x86_64", "aarch64").
	Arch string
	// DomainType is the libvirt domain type (e.g. "kvm", "qemu").
	DomainType string
	// CPUs is the host CPU count, used to range-check a cpuset. Zero means unknown.
	CPUs int
}

// defaultHostCaps is the fallback when the driver cannot report domain
// capabilities (e.g. test:///default). q35 gives the seed cdrom a native SATA bus.
var defaultHostCaps = hostCaps{
	Emulator:   "/usr/bin/qemu-system-x86_64",
	Machine:    "q35",
	Arch:       "x86_64",
	DomainType: "kvm",
}

type domainCapsXML struct {
	Path    string `xml:"path"`
	Domain  string `xml:"domain"`
	Machine string `xml:"machine"`
	Arch    string `xml:"arch"`
}

// parseDomainCaps reads a domainCapabilities document. Missing or unparseable
// fields fall back to defaultHostCaps, so it cannot fail.
func parseDomainCaps(xmlStr string) hostCaps {
	var doc domainCapsXML
	_ = xml.Unmarshal([]byte(xmlStr), &doc)

	caps := defaultHostCaps // start from defaults

	if doc.Path != "" {
		caps.Emulator = doc.Path
	}
	if doc.Domain != "" {
		caps.DomainType = doc.Domain
	}
	if doc.Machine != "" {
		caps.Machine = doc.Machine
	}
	if doc.Arch != "" {
		caps.Arch = doc.Arch
	}

	return caps
}

// VMCreateParams holds all inputs required to define and start a new domain.
type VMCreateParams struct {
	Name string
	// ImagePath is the full path to the cached base qcow2/img backing file.
	ImagePath string
	// DiskPath is the full path where the VM's own qcow2 overlay disk lives.
	DiskPath string
	// CloudInitISO is the full path to the cloud-init NoCloud seed ISO.
	CloudInitISO string
	// VCPUs is the number of virtual CPUs to assign.
	VCPUs int
	// MemoryMB is the amount of RAM in mebibytes.
	MemoryMB int
	// NetworkMode is metadata.NetworkNAT or metadata.NetworkBridge.
	NetworkMode string
	// Bridge is the host bridge for bridge mode; empty is metadata.DefaultBridge.
	Bridge string
	// VLANID tags the guest's frames; 0 is untagged. Bridge mode only.
	VLANID int
	// VLANUplink is a provisioned VLAN's host interface; empty if unprovisioned.
	VLANUplink string
	// Firmware is metadata.FirmwareUEFI, or empty for BIOS.
	Firmware string
	// SecureBoot is only meaningful alongside FirmwareUEFI.
	SecureBoot bool
	// TPM adds an emulated TPM 2.0, which needs swtpm on the host.
	TPM bool
	// CPUMode is metadata.CPUHostPassthrough, or empty for host-model.
	CPUMode string
	// CPUTopology fixes the guest's socket/core/thread layout; nil derives it.
	CPUTopology *metadata.CPUTopology
	// CPUSet pins the VM to host CPUs, in libvirt's cpuset syntax.
	CPUSet string
	// PCIDevices and USBDevices are host devices assigned to the guest, already
	// resolved against the host inventory; this layer only checks their form.
	PCIDevices []metadata.PCIDevice
	USBDevices []metadata.USBDevice
}

// entry converts create params to a spec entry, so create renders through
// DomainSpecFromEntry like every rebuild does and the two cannot drift.
func (p VMCreateParams) entry() metadata.VMEntry {
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

// ErrNotConnected is returned, wrapped, by every Manager method when libvirt
// cannot be reached. It is transient, and internal/api maps it to 503.
var ErrNotConnected = errors.New("vm: not connected to libvirt")

// ErrIncompleteSpec means a spec entry lacks the fields needed to build a
// machine from it. That needs a human, so the reconcile loop must not retry.
var ErrIncompleteSpec = errors.New("vm: spec does not describe a complete machine")

// ErrNotRunning means a stop found the domain already off.
var ErrNotRunning = errors.New("vm: domain is not running")

// ErrVMNotFound means no domain has the name a call asked for. It is returned
// wrapped, with the name, and internal/api maps it to 404.
var ErrVMNotFound = errors.New("no such VM")

// stopIsNoOp reports whether d is already inactive. It reads the state because
// the drivers disagree on the stop error's code (VIR_ERR_OPERATION_INVALID on
// qemu:///system, VIR_ERR_INTERNAL_ERROR on test:///default).
func stopIsNoOp(lv *libvirt.Libvirt, d libvirt.Domain) bool {
	state, _, err := lv.DomainGetState(d, 0)
	if err != nil {
		return false // unknown state is not evidence; let the RPC decide
	}
	switch libvirt.DomainState(state) {
	case libvirt.DomainShutoff, libvirt.DomainCrashed:
		return true
	default:
		return false
	}
}

// Manager handles VM lifecycle against libvirt. Its connection is lazy and
// reconnects on demand, so methods read lv through conn(), never m.lv.
type Manager struct {
	uri string

	mu   sync.RWMutex
	lv   *libvirt.Libvirt
	caps hostCaps // guarded by mu; (re)written on every successful Connect
}

// NewLazy returns a Manager for uri without dialing, so the daemon can start
// before libvirtd is up. Failures surface later as ErrNotConnected.
func NewLazy(uri string) *Manager {
	return &Manager{uri: uri}
}

// New is NewLazy followed by Connect, for callers that want to fail fast:
// tests and `nanocloud doctor`. Everything else should use NewLazy.
func New(uri string) (*Manager, error) {
	m := NewLazy(uri)
	if err := m.Connect(); err != nil {
		return nil, err
	}
	return m, nil
}

// Connect dials libvirt and installs the handle; it is a no-op while
// connected. Each attempt dials a new *libvirt.Libvirt: reusing one through
// ConnectToURI races, because go-libvirt reassigns its disconnected channel
// without a lock.
func (m *Manager) Connect() error {
	_, err := m.connect()
	return err
}

// connect is Connect returning the installed handle. conn() uses the return
// value because a concurrent Close can nil m.lv once the lock is released.
func (m *Manager) connect() (*libvirt.Libvirt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Another caller may have reconnected while this one waited. Dialing again
	// would disconnect that handle under the calls already using it.
	if m.lv != nil && m.lv.IsConnected() {
		return m.lv, nil
	}

	parsed, err := url.Parse(m.uri)
	if err != nil {
		return nil, fmt.Errorf("%w: parse libvirt URI %q: %v", ErrNotConnected, m.uri, err)
	}

	lv, err := libvirt.ConnectToURI(parsed)
	if err != nil {
		return nil, fmt.Errorf("%w: connect to libvirt %q: %v", ErrNotConnected, m.uri, err)
	}

	if m.lv != nil {
		_ = m.lv.Disconnect()
	}
	m.lv = lv
	m.caps = detectHostCaps(lv)
	return lv, nil
}

// conn returns the live handle, dialing if needed. Use the returned handle for
// the whole call: a concurrent reconnect can replace m.lv.
func (m *Manager) conn() (*libvirt.Libvirt, error) {
	m.mu.RLock()
	lv := m.lv
	connected := lv != nil && lv.IsConnected()
	m.mu.RUnlock()
	if connected {
		return lv, nil
	}

	return m.connect()
}

// Connected reports whether a live connection is held, without dialing.
func (m *Manager) Connected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lv != nil && m.lv.IsConnected()
}

// alreadyDisconnected is what Disconnected returns before any connection, so
// a select on it fires at once instead of blocking forever.
var alreadyDisconnected = closedChan()

func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Disconnected returns a channel closed once the current connection is lost.
// It does not dial.
func (m *Manager) Disconnected() <-chan struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.lv == nil {
		return alreadyDisconnected
	}
	return m.lv.Disconnected()
}

// detectHostCaps reads host capabilities over lv, falling back to
// defaultHostCaps where the driver cannot answer (e.g. test:///default).
func detectHostCaps(lv *libvirt.Libvirt) hostCaps {
	caps := defaultHostCaps
	if xmlStr, err := lv.ConnectGetDomainCapabilities(nil, nil, nil, nil, 0); err != nil {
		log.Printf("vm: ConnectGetDomainCapabilities unavailable (%v); using defaults", err)
	} else {
		caps = parseDomainCaps(xmlStr)
	}

	// An unknown CPU count only skips the cpuset range check; libvirt still
	// refuses an impossible cpuset at define time.
	if _, _, cpus, _, _, _, _, _, err := lv.NodeGetInfo(); err != nil {
		log.Printf("vm: NodeGetInfo unavailable (%v); cpu pinning will not be range-checked", err)
	} else {
		caps.CPUs = int(cpus)
	}
	return caps
}

// HostCPUs returns the host CPU count seen at connect time, or zero if
// unknown, which ValidateCPU treats as "syntax only". It dials on demand so a
// cold Manager does not silently skip the range check.
func (m *Manager) HostCPUs() int {
	if _, err := m.conn(); err != nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.caps.CPUs
}

// Close disconnects from libvirt. It is safe on a Manager that never
// connected, and safe to call more than once.
func (m *Manager) Close() error {
	m.mu.Lock()
	lv := m.lv
	m.lv = nil
	m.mu.Unlock()

	if lv == nil {
		return nil
	}
	if err := lv.Disconnect(); err != nil {
		return fmt.Errorf("vm: disconnect from libvirt: %w", err)
	}
	return nil
}

// List returns all domains, active and inactive. A domain whose info cannot
// be read is still listed, with state "unknown" and zero resources.
func (m *Manager) List() ([]VM, error) {
	lv, err := m.conn()
	if err != nil {
		return nil, err
	}

	flags := libvirt.ConnectListDomainsActive | libvirt.ConnectListDomainsInactive
	domains, _, err := lv.ConnectListAllDomains(1, flags)
	if err != nil {
		return nil, fmt.Errorf("vm: list domains: %w", err)
	}

	vms := make([]VM, 0, len(domains))
	for _, d := range domains {
		v := VM{
			ID:   d.ID,
			Name: d.Name,
		}

		state, _, mem, nrVCPU, _, err := lv.DomainGetInfo(d)
		if err != nil {
			v.State = "unknown"
		} else {
			v.State = domainStateString(libvirt.DomainState(state))
			v.MemoryKB = mem
			v.VCPUs = nrVCPU
		}

		vms = append(vms, v)
	}

	return vms, nil
}

// Get returns the named VM with its state and resources.
func (m *Manager) Get(name string) (*VM, error) {
	lv, d, err := m.lookup(name, "")
	if err != nil {
		return nil, err
	}

	v := &VM{
		ID:   d.ID,
		Name: d.Name,
	}

	state, _, mem, nrVCPU, _, err := lv.DomainGetInfo(d)
	if err != nil {
		v.State = "unknown"
	} else {
		v.State = domainStateString(libvirt.DomainState(state))
		v.MemoryKB = mem
		v.VCPUs = nrVCPU
	}

	return v, nil
}

// PrimaryIP returns the VM's first usable IPv4 address and whether the guest
// confirmed it (see primaryIPFrom). No address yet returns "", not an error.
func (m *Manager) PrimaryIP(name string) (ip string, confirmed bool) {
	lv, err := m.conn()
	if err != nil {
		return "", false
	}
	d, err := lv.DomainLookupByName(name)
	if err != nil {
		return "", false
	}
	return primaryIPFrom(func(src libvirt.DomainInterfaceAddressesSource) []string {
		ifaces, err := lv.DomainInterfaceAddresses(d, uint32(src), 0)
		if err != nil {
			return nil
		}
		if src != libvirt.DomainInterfaceAddressesSrcAgent || len(ifaces) == 0 {
			return ipv4sOf(ifaces)
		}
		// Fetched only on the agent path, so the ARP-confirmed case stays one
		// round-trip. A failed read leaves mac empty: the name-based fallback.
		var mac string
		if doc, err := lv.DomainGetXMLDesc(d, 0); err == nil {
			if id, err := ParseDomainIdentity(doc); err == nil {
				mac = id.MAC
			}
		}
		return agentIPv4s(ifaces, mac)
	})
}

// agentIPv4s picks the guest's own address from the agent's reply, which also
// lists interfaces like cni0 or docker0 that are unreachable from outside. It
// matches the domain's MAC, falling back to skipping such names.
func agentIPv4s(ifaces []libvirt.DomainInterface, mac string) []string {
	if mac != "" {
		for _, iface := range ifaces {
			if len(iface.Hwaddr) > 0 && strings.EqualFold(iface.Hwaddr[0], mac) {
				return ipv4sOf([]libvirt.DomainInterface{iface})
			}
		}
	}
	var physical []libvirt.DomainInterface
	for _, iface := range ifaces {
		if !isVirtualIfaceName(iface.Name) {
			physical = append(physical, iface)
		}
	}
	return ipv4sOf(physical)
}

// virtualIfacePrefixes are the guest-side links container runtimes and
// overlays create; none is how the host reaches the guest.
var virtualIfacePrefixes = []string{"lo", "docker", "cni", "flannel", "veth", "br-"}

func isVirtualIfaceName(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// primaryIPFrom is PrimaryIP's decision, split out for testing. ARP and the
// agent need the guest's network stack to have answered, so either confirms.
// The lease comes from dnsmasq's status file, keyed by the persisted MAC, and
// survives a host power cut, so it can name the address of a guest hung in its
// bootloader. It is tried last and never confirms.
func primaryIPFrom(ipsFrom func(libvirt.DomainInterfaceAddressesSource) []string) (string, bool) {
	for _, src := range []libvirt.DomainInterfaceAddressesSource{
		libvirt.DomainInterfaceAddressesSrcArp,
		libvirt.DomainInterfaceAddressesSrcAgent,
	} {
		if ips := ipsFrom(src); len(ips) > 0 {
			return ips[0], true
		}
	}
	if ips := ipsFrom(libvirt.DomainInterfaceAddressesSrcLease); len(ips) > 0 {
		return ips[0], false
	}
	return "", false
}

// ipv4sOf flattens one source's reply down to usable IPv4 addresses.
func ipv4sOf(ifaces []libvirt.DomainInterface) []string {
	var out []string
	for _, iface := range ifaces {
		for _, addr := range iface.Addrs {
			if libvirt.IPAddrType(addr.Type) != libvirt.IPAddrTypeIpv4 {
				continue
			}
			if addr.Addr == "" || strings.HasPrefix(addr.Addr, "127.") || strings.HasPrefix(addr.Addr, "169.254.") {
				continue
			}
			out = append(out, addr.Addr)
		}
	}
	return out
}

// Define registers a VM with libvirt without starting it.
func (m *Manager) Define(params VMCreateParams) error {
	e := params.entry()
	if err := m.validateSpec(e); err != nil {
		return err
	}
	return m.defineSpec(DomainSpecFromEntry(e, params.DiskPath, params.CloudInitISO))
}

// validateSpec refuses a spec libvirt would not accept. internal/api checks
// first; this catches a hand-edited metadata.json.
func (m *Manager) validateSpec(e metadata.VMEntry) error {
	if err := ValidateHostDevices(e); err != nil {
		return err
	}
	return ValidateCPU(e, m.HostCPUs())
}

// DefineFromSpec registers the domain the spec describes, without starting it,
// to rebuild a domain that vanished from libvirt. A spec with zero VCPUs or
// MemoryMB gets ErrIncompleteSpec instead of libvirt's misleading <numa> error.
func (m *Manager) DefineFromSpec(e metadata.VMEntry, diskPath, isoPath string) error {
	if e.VCPUs <= 0 || e.MemoryMB <= 0 {
		return fmt.Errorf("vm: cannot rebuild %q: %w (vcpus=%d memory_mb=%d)", e.ID, ErrIncompleteSpec, e.VCPUs, e.MemoryMB)
	}
	if err := m.validateSpec(e); err != nil {
		return err
	}
	return m.defineSpec(DomainSpecFromEntry(e, diskPath, isoPath))
}

// RedefineFromSpec rebuilds an existing domain in place; changes take effect at
// the guest's next boot. The live domain's UUID and MAC override the spec's:
// without the UUID libvirt refuses the definition, and without the MAC the
// guest comes back on a new DHCP lease and a different IP.
func (m *Manager) RedefineFromSpec(e metadata.VMEntry, diskPath, isoPath string) error {
	if e.VCPUs <= 0 || e.MemoryMB <= 0 {
		return fmt.Errorf("vm: cannot rebuild %q: %w (vcpus=%d memory_mb=%d)", e.ID, ErrIncompleteSpec, e.VCPUs, e.MemoryMB)
	}
	if err := m.validateSpec(e); err != nil {
		return err
	}

	id, err := m.Identity(e.ID)
	if err != nil {
		return err
	}

	s := DomainSpecFromEntry(e, diskPath, isoPath)
	applyIdentity(&s, id)
	return m.defineSpec(s)
}

// Identity reads the UUID and MAC libvirt assigned a domain. They exist only
// in the domain, so a later rebuild depends on reading them while it exists.
func (m *Manager) Identity(name string) (DomainIdentity, error) {
	lv, d, err := m.lookup(name, " to read its identity")
	if err != nil {
		return DomainIdentity{}, err
	}
	doc, err := lv.DomainGetXMLDesc(d, 0)
	if err != nil {
		return DomainIdentity{}, fmt.Errorf("vm: read XML of %q: %w", name, err)
	}
	return ParseDomainIdentity(doc)
}

// defineSpec fills in host capabilities and hands the document to libvirt.
func (m *Manager) defineSpec(s DomainSpec) error {
	lv, err := m.conn()
	if err != nil {
		return err
	}
	m.applyCaps(&s)

	doc, err := BuildDomainXML(s)
	if err != nil {
		return err
	}
	if _, err := lv.DomainDefineXML(doc); err != nil {
		return fmt.Errorf("vm: define domain %q: %w", s.Name, err)
	}
	return nil
}

// applyCaps fills host-specific fields from the detected capabilities. Values
// already set win, which lets a test pin a machine type.
func (m *Manager) applyCaps(s *DomainSpec) {
	m.mu.RLock()
	caps := m.caps
	m.mu.RUnlock()

	if s.Type == "" {
		s.Type = caps.DomainType
	}
	if s.OS.Type.Arch == "" {
		s.OS.Type.Arch = caps.Arch
	}
	if s.OS.Type.Machine == "" {
		s.OS.Type.Machine = caps.Machine
	}
	if s.Devices.Emulator == "" {
		s.Devices.Emulator = caps.Emulator
	}

	// Secure boot needs SMM, which on x86 only q35 has. This runs here, once the
	// arch is known, because QEMU x86 defaults to i440fx even though the fallback
	// caps say q35.
	if s.Features.SMM != nil && s.OS.Type.Arch == "x86_64" && !strings.Contains(s.OS.Type.Machine, "q35") {
		s.OS.Type.Machine = "q35"
	}
}

// Create defines and immediately starts a VM from the given params.
func (m *Manager) Create(params VMCreateParams) error {
	if err := m.Define(params); err != nil {
		return err
	}
	if err := m.Start(params.Name); err != nil {
		// Best-effort; the start error is the one to report.
		if delErr := m.Delete(params.Name); delErr != nil {
			log.Printf("vm: create %q: cleanup undefine after failed start also failed: %v", params.Name, delErr)
		}
		return fmt.Errorf("vm: start domain %q after define: %w", params.Name, err)
	}
	return nil
}

// ObservedDomain is one domain's live state for the reconcile loop. State is
// raw, so Crashed stays distinct from Shutoff; Reason separates an in-guest
// poweroff from an external kill.
type ObservedDomain struct {
	Name   string
	State  libvirt.DomainState
	Reason int32
}

// Observe lists every domain libvirt knows, active and inactive. A domain
// missing from the result is not defined at all.
func (m *Manager) Observe() ([]ObservedDomain, error) {
	lv, err := m.conn()
	if err != nil {
		return nil, err
	}

	flags := libvirt.ConnectListDomainsActive | libvirt.ConnectListDomainsInactive
	domains, _, err := lv.ConnectListAllDomains(1, flags)
	if err != nil {
		return nil, fmt.Errorf("vm: observe domains: %w", err)
	}

	out := make([]ObservedDomain, 0, len(domains))
	for _, d := range domains {
		state, reason, err := lv.DomainGetState(d, 0)
		if err != nil {
			// Dropping the domain would read as "not defined", the loop's terminal
			// Degraded verdict. DomainNostate means "look again next tick".
			out = append(out, ObservedDomain{Name: d.Name})
			continue
		}
		out = append(out, ObservedDomain{Name: d.Name, State: libvirt.DomainState(state), Reason: reason})
	}
	return out, nil
}

// lookup dials libvirt and finds the domain called name. why finishes the
// lookup error's "vm: lookup %q" with what the domain was wanted for.
func (m *Manager) lookup(name, why string) (*libvirt.Libvirt, libvirt.Domain, error) {
	lv, err := m.conn()
	if err != nil {
		return nil, libvirt.Domain{}, err
	}
	d, err := lv.DomainLookupByName(name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return nil, libvirt.Domain{}, fmt.Errorf("vm: lookup %q%s: %w", name, why, ErrVMNotFound)
		}
		return nil, libvirt.Domain{}, fmt.Errorf("vm: lookup %q%s: %w", name, why, err)
	}
	return lv, d, nil
}

// Start boots a domain that is currently stopped (defined but not running).
func (m *Manager) Start(name string) error {
	lv, d, err := m.lookup(name, " for start")
	if err != nil {
		return err
	}
	if err := lv.DomainCreate(d); err != nil {
		return fmt.Errorf("vm: start %q: %w", name, err)
	}
	return nil
}

// Stop sends an ACPI shutdown request. The guest may take a while, or ignore
// it; Destroy forces it off.
func (m *Manager) Stop(name string) error {
	lv, d, err := m.lookup(name, " for stop")
	if err != nil {
		return err
	}
	if stopIsNoOp(lv, d) {
		return fmt.Errorf("vm: shutdown %q: %w", name, ErrNotRunning)
	}
	if err := lv.DomainShutdown(d); err != nil {
		return fmt.Errorf("vm: shutdown %q: %w", name, err)
	}
	return nil
}

// Destroy force-stops a VM immediately, like pulling the power cord.
func (m *Manager) Destroy(name string) error {
	lv, d, err := m.lookup(name, " for destroy")
	if err != nil {
		return err
	}
	if stopIsNoOp(lv, d) {
		return fmt.Errorf("vm: destroy %q: %w", name, ErrNotRunning)
	}
	if err := lv.DomainDestroy(d); err != nil {
		return fmt.Errorf("vm: destroy %q: %w", name, err)
	}
	return nil
}

// undefineFlags covers what makes a bare DomainUndefine fail: snapshot
// metadata and a managed-save image. Snapshot data stays in the qcow2.
//
// Add no speculative flags: drivers reject unsupported flags instead of
// ignoring them, failing the whole undefine. See undefineFlagsFor for NVRAM.
const undefineFlags = libvirt.DomainUndefineSnapshotsMetadata |
	libvirt.DomainUndefineManagedSave

// undefineFlagsFor adds DomainUndefineNvram only when the domain's own XML
// declares NVRAM: a UEFI domain's undefine fails without it, a BIOS domain's
// with it. A parse failure gets the base flags.
func undefineFlagsFor(domainXML string) libvirt.DomainUndefineFlagsValues {
	if hasNVRAM(domainXML) {
		return undefineFlags | libvirt.DomainUndefineNvram
	}
	return undefineFlags
}

// hasNVRAM reports whether a domain document declares an NVRAM store.
func hasNVRAM(domainXML string) bool {
	var doc struct {
		OS struct {
			NVRAM *string `xml:"nvram"`
		} `xml:"os"`
	}
	if err := xml.Unmarshal([]byte(domainXML), &doc); err != nil {
		return false
	}
	return doc.OS.NVRAM != nil
}

// Delete undefines a stopped domain with its snapshot metadata and managed-save
// image; the disk and seed ISO are the caller's. A domain libvirt does not know
// counts as deleted, so a degraded VM can still be cleaned up.
func (m *Manager) Delete(name string) error {
	lv, err := m.conn()
	if err != nil {
		return err
	}
	d, err := lv.DomainLookupByName(name)
	if err != nil {
		if libvirt.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("vm: lookup %q for delete: %w", name, err)
	}
	// A failed read gets the base flags, which still work for BIOS domains.
	domainXML, err := lv.DomainGetXMLDesc(d, 0)
	if err != nil {
		log.Printf("vm: read XML of %q before undefine (%v); assuming no NVRAM", name, err)
	}
	if err := lv.DomainUndefineFlags(d, undefineFlagsFor(domainXML)); err != nil {
		return fmt.Errorf("vm: undefine %q: %w", name, err)
	}
	return nil
}

// ActiveHardware is a domain's current configuration, reduced to what a spec
// can change. It is the live side of every restart-required comparison.
type ActiveHardware struct {
	HostDevs    []HostDevRef
	CPU         CPUShape
	Network     NetworkShape
	DiskTargets []string
}

// ActiveHardware reads what the domain currently has. Flags are 0, not
// DomainXMLInactive: libvirt then returns the running configuration while the
// domain runs and the persistent one once it stops, which is exactly the
// restart-required distinction.
func (m *Manager) ActiveHardware(name string) (ActiveHardware, error) {
	lv, d, err := m.lookup(name, "")
	if err != nil {
		return ActiveHardware{}, err
	}
	doc, err := lv.DomainGetXMLDesc(d, 0)
	if err != nil {
		return ActiveHardware{}, fmt.Errorf("vm: read XML of %q: %w", name, err)
	}
	return parseActiveHardware(doc)
}

// parseActiveHardware ignores the guest-agent channel on purpose: older domains
// gain it on redefine and lack it until restarted, and it is not hardware
// anyone asked for, so it must not raise restart_required.
func parseActiveHardware(doc string) (ActiveHardware, error) {
	var (
		hw  ActiveHardware
		err error
	)
	if hw.HostDevs, err = ParseDomainHostDevs(doc); err != nil {
		return ActiveHardware{}, err
	}
	if hw.CPU, err = ParseDomainCPUShape(doc); err != nil {
		return ActiveHardware{}, err
	}
	if hw.Network, err = ParseDomainNetworkShape(doc); err != nil {
		return ActiveHardware{}, err
	}
	if hw.DiskTargets, err = ParseDomainDiskTargets(doc); err != nil {
		return ActiveHardware{}, err
	}
	return hw, nil
}

// Exists reports whether a domain with the given name is defined. A libvirt
// "no such domain" is (false, nil); any other failure is an error.
func (m *Manager) Exists(name string) (bool, error) {
	lv, err := m.conn()
	if err != nil {
		return false, err
	}
	if _, err := lv.DomainLookupByName(name); err != nil {
		if libvirt.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("vm: lookup %q: %w", name, err)
	}
	return true, nil
}

// domainGraphicsXML is the part of the live domain XML that holds VNC details.
type domainGraphicsXML struct {
	Graphics struct {
		Type   string `xml:"type,attr"`
		Port   int    `xml:"port,attr"`
		Listen string `xml:"listen,attr"`
	} `xml:"devices>graphics"`
}

// VNCAddress returns the host:port of a running VM's VNC console, from the
// live XML or, failing that, `virsh vncdisplay`.
func (m *Manager) VNCAddress(name string) (string, error) {
	lv, d, err := m.lookup(name, " for console")
	if err != nil {
		return "", err
	}

	state, _, _, _, _, err := lv.DomainGetInfo(d)
	if err != nil || domainStateString(libvirt.DomainState(state)) != "running" {
		return "", fmt.Errorf("vm: %q is not running", name)
	}

	xmlDesc, err := lv.DomainGetXMLDesc(d, 0)
	if err != nil {
		return "", fmt.Errorf("vm: get xml for %q: %w", name, err)
	}

	var doc domainGraphicsXML
	if err := xml.Unmarshal([]byte(xmlDesc), &doc); err != nil {
		return "", fmt.Errorf("vm: parse domain xml for %q: %w", name, err)
	}

	if doc.Graphics.Port > 0 {
		listen := doc.Graphics.Listen
		if listen == "" {
			listen = "127.0.0.1"
		}
		return fmt.Sprintf("%s:%d", listen, doc.Graphics.Port), nil
	}

	out, verr := m.virsh("vncdisplay", name)
	if verr != nil {
		return "", fmt.Errorf("vm: no vnc port available for %q: %w", name, verr)
	}
	return parseVNCDisplay(string(out))
}

// parseVNCDisplay converts `virsh vncdisplay` output (":0", "127.0.0.1:1") to
// host:port; display N is TCP port 5900+N.
func parseVNCDisplay(display string) (string, error) {
	display = strings.TrimSpace(display)
	host, offsetStr, ok := strings.Cut(display, ":")
	if !ok {
		return "", fmt.Errorf("vm: unrecognized vncdisplay output %q", display)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	offset, err := strconv.Atoi(strings.TrimSpace(offsetStr))
	if err != nil {
		return "", fmt.Errorf("vm: unrecognized vncdisplay port offset in %q: %w", display, err)
	}
	return fmt.Sprintf("%s:%d", host, 5900+offset), nil
}

// Snapshot describes a single domain snapshot as returned by ListSnapshots.
type Snapshot struct {
	Name string `json:"name"`
	// Description is the optional human-readable description set at creation time.
	Description string `json:"description"`
	// CreatedAt is the UTC time at which the snapshot was taken.
	CreatedAt time.Time `json:"created_at"`
	// State is the domain state at snapshot time: "running" or "shutoff".
	State string `json:"state"`
}

type snapshotXML struct {
	Name         string `xml:"name"`
	Description  string `xml:"description"`
	CreationTime int64  `xml:"creationTime"`
	State        string `xml:"state"`
}

// ParseSnapshotXML parses libvirt's XML description of one snapshot.
func ParseSnapshotXML(xmlStr string) (Snapshot, error) {
	var sx snapshotXML
	if err := xml.Unmarshal([]byte(xmlStr), &sx); err != nil {
		return Snapshot{}, fmt.Errorf("vm: parse snapshot XML: %w", err)
	}
	return Snapshot{
		Name:        sx.Name,
		Description: sx.Description,
		CreatedAt:   time.Unix(sx.CreationTime, 0).UTC(),
		State:       sx.State,
	}, nil
}

// ListSnapshots returns the domain's snapshots, oldest first, or an empty slice
// when there are none. Drivers without snapshot support return an error.
func (m *Manager) ListSnapshots(vmName string) ([]Snapshot, error) {
	lv, d, err := m.lookup(vmName, " for list snapshots")
	if err != nil {
		return nil, err
	}

	count, err := lv.DomainSnapshotNum(d, 0)
	if err != nil {
		return nil, fmt.Errorf("vm: snapshot count for %q: %w", vmName, err)
	}
	if count == 0 {
		return []Snapshot{}, nil
	}

	names, err := lv.DomainSnapshotListNames(d, count, 0)
	if err != nil {
		return nil, fmt.Errorf("vm: list snapshot names for %q: %w", vmName, err)
	}

	snaps := make([]Snapshot, 0, len(names))
	for _, name := range names {
		snap, err := lv.DomainSnapshotLookupByName(d, name, 0)
		if err != nil {
			return nil, fmt.Errorf("vm: lookup snapshot %q on %q: %w", name, vmName, err)
		}
		xmlStr, err := lv.DomainSnapshotGetXMLDesc(snap, 0)
		if err != nil {
			return nil, fmt.Errorf("vm: get XML for snapshot %q on %q: %w", name, vmName, err)
		}
		s, err := ParseSnapshotXML(xmlStr)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}

	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].CreatedAt.Before(snaps[j].CreatedAt)
	})
	return snaps, nil
}

// CreateSnapshot takes a snapshot of the named domain. description is optional.
func (m *Manager) CreateSnapshot(vmName, snapName, description string) error {
	lv, d, err := m.lookup(vmName, " for create snapshot")
	if err != nil {
		return err
	}

	snapXML := fmt.Sprintf("<domainsnapshot><name>%s</name><description>%s</description></domainsnapshot>",
		snapName, description)
	if _, err := lv.DomainSnapshotCreateXML(d, snapXML, 0); err != nil {
		return fmt.Errorf("vm: create snapshot %q on %q: %w", snapName, vmName, err)
	}
	return nil
}

// DeleteSnapshot removes the named snapshot from the domain.
func (m *Manager) DeleteSnapshot(vmName, snapName string) error {
	lv, d, err := m.lookup(vmName, " for delete snapshot")
	if err != nil {
		return err
	}
	snap, err := lv.DomainSnapshotLookupByName(d, snapName, 0)
	if err != nil {
		return fmt.Errorf("vm: lookup snapshot %q on %q: %w", snapName, vmName, err)
	}
	if err := lv.DomainSnapshotDelete(snap, 0); err != nil {
		return fmt.Errorf("vm: delete snapshot %q on %q: %w", snapName, vmName, err)
	}
	return nil
}

// RestoreSnapshot reverts the domain to a snapshot and always leaves it
// stopped. libvirt has no revert flag for that, so the domain is forced off
// after the revert if it came back active.
func (m *Manager) RestoreSnapshot(vmName, snapName string) error {
	lv, d, err := m.lookup(vmName, " for restore snapshot")
	if err != nil {
		return err
	}

	state, _, _, _, _, err := lv.DomainGetInfo(d)
	if err != nil {
		return fmt.Errorf("vm: get state of %q before restore: %w", vmName, err)
	}
	if libvirt.DomainState(state) == libvirt.DomainRunning ||
		libvirt.DomainState(state) == libvirt.DomainBlocked {
		if destroyErr := lv.DomainDestroy(d); destroyErr != nil {
			return fmt.Errorf("vm: force-stop %q before restore: %w", vmName, destroyErr)
		}
	}

	snap, err := lv.DomainSnapshotLookupByName(d, snapName, 0)
	if err != nil {
		return fmt.Errorf("vm: lookup snapshot %q on %q: %w", snapName, vmName, err)
	}
	if err = lv.DomainRevertToSnapshot(snap, 0); err != nil {
		return fmt.Errorf("vm: revert %q to snapshot %q: %w", vmName, snapName, err)
	}

	postState, _, _, _, _, err := lv.DomainGetInfo(d)
	if err != nil {
		return fmt.Errorf("vm: get state of %q after restore: %w", vmName, err)
	}
	// Check for "not Shutoff" rather than listing active states.
	if libvirt.DomainState(postState) != libvirt.DomainShutoff {
		if destroyErr := lv.DomainDestroy(d); destroyErr != nil {
			// It may have reached shutoff on its own since the check.
			finalState, infoErr := m.Get(vmName)
			if infoErr == nil && finalState != nil && finalState.State == "stopped" {
				return nil
			}
			return fmt.Errorf("vm: force-stop %q after restore: %w", vmName, destroyErr)
		}
	}
	return nil
}

// ListNodeDeviceXML returns the raw XML of every PCI and USB node device.
// Parsing is the caller's, so it can be tested from fixtures.
func (m *Manager) ListNodeDeviceXML() ([]string, error) {
	lv, err := m.conn()
	if err != nil {
		return nil, err
	}

	// Only PCI and USB, since each description is another round-trip. An
	// unrecognised bit here is not rejected: it returns an empty list, so a wrong
	// mask silently blanks the inventory.
	const capFilter = uint32(libvirt.ConnectListNodeDevicesCapPciDev | libvirt.ConnectListNodeDevicesCapUsbDev)

	devs, _, err := lv.ConnectListAllNodeDevices(1, capFilter)
	if err != nil {
		return nil, fmt.Errorf("vm: list node devices: %w", err)
	}

	out := make([]string, 0, len(devs))
	for _, d := range devs {
		xmlStr, err := lv.NodeDeviceGetXMLDesc(d.Name, 0)
		if err != nil {
			// Unplugged since the listing; skip rather than fail the inventory.
			log.Printf("vm: node device %q description unavailable: %v", d.Name, err)
			continue
		}
		out = append(out, xmlStr)
	}
	return out, nil
}

// domainStateString maps a libvirt state to the API's state string.
func domainStateString(s libvirt.DomainState) string {
	switch s {
	case libvirt.DomainRunning, libvirt.DomainBlocked:
		return "running"
	case libvirt.DomainPaused, libvirt.DomainPmsuspended:
		return "paused"
	case libvirt.DomainShutdown, libvirt.DomainShutoff, libvirt.DomainCrashed:
		return "stopped"
	default:
		return "unknown"
	}
}

// virsh runs a virsh command against the Manager's URI. It is the escape hatch
// for what go-libvirt does not cover; keep its callers in this package.
func (m *Manager) virsh(args ...string) ([]byte, error) {
	fullArgs := append([]string{"--connect", m.uri}, args...)
	cmd := exec.Command("virsh", fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("vm: virsh %v: %w — stderr: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
