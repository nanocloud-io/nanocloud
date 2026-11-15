// Package hypervisor is the interface between the API, reconcile loop and
// stats sampler and the backend that runs VMs: *vm.Manager (libvirt/QEMU on
// Linux) or *vzvm.Manager (Virtualization.framework on macOS).
//
// It uses internal/vm's types, including libvirt.DomainState, so the VZ
// backend reports its machines with the same constants the reconcile loop
// already decides on. Features only one backend has are optional interfaces,
// found by type assertion and advertised through Capabilities.
package hypervisor

import (
	"errors"
	"io"

	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/vm"
	"github.com/nanocloud-io/nanocloud/internal/vswitch"
)

// Backend is everything internal/api, internal/reconcile and internal/stats
// need from a hypervisor. reconcile.Hypervisor and stats.Hypervisor are
// subsets of it.
type Backend interface {
	Connect() error
	Close() error
	HostCPUs() int

	List() ([]vm.VM, error)
	Get(name string) (*vm.VM, error)
	Exists(name string) (bool, error)
	Observe() ([]vm.ObservedDomain, error)
	Identity(name string) (vm.DomainIdentity, error)
	PrimaryIP(name string) (ip string, confirmed bool)
	ActiveHardware(name string) (vm.ActiveHardware, error)
	SampleAll() ([]vm.VMSample, error)

	Create(params vm.VMCreateParams) error
	DefineFromSpec(e metadata.VMEntry, diskPath, isoPath string) error
	RedefineFromSpec(e metadata.VMEntry, diskPath, isoPath string) error
	Start(name string) error
	Stop(name string) error
	Destroy(name string) error
	Delete(name string) error
}

// Capable is implemented by a backend that reports its own Capabilities.
// A backend that does not is assumed to be libvirt (see CapabilitiesOf).
type Capable interface {
	Capabilities() Capabilities
}

// Snapshotter is a backend that can take and restore point-in-time snapshots.
type Snapshotter interface {
	ListSnapshots(vmName string) ([]vm.Snapshot, error)
	CreateSnapshot(vmName, snapName, description string) error
	DeleteSnapshot(vmName, snapName string) error
	RestoreSnapshot(vmName, snapName string) error
}

// VNCConsoler is a backend whose guests expose a VNC server on a host TCP
// address. GET /vms/{id}/console proxies RFB to it.
type VNCConsoler interface {
	VNCAddress(name string) (string, error)
}

// SerialConsoler is a backend whose guests expose a serial console as a byte
// stream. GET /vms/{id}/console proxies the stream to the browser.
type SerialConsoler interface {
	OpenSerial(name string) (io.ReadWriteCloser, error)
}

// HostDeviceLister is a backend that can enumerate host PCI/USB devices for
// passthrough, as libvirt node-device XML.
type HostDeviceLister interface {
	ListNodeDeviceXML() ([]string, error)
}

// Switcher is a backend that links its VMs into its own virtual switch, as
// on macOS.
type Switcher interface {
	// SwitchStatus reports the switch and every VM's port on it, or
	// vswitch.ErrNoSwitch when this host's VMs are not on one.
	SwitchStatus() (vswitch.Report, error)
}

// Capabilities tells the UI which features this daemon's backend offers, so
// it can hide what would only fail. Served under GET /system.
type Capabilities struct {
	// Backend names the implementation: "libvirt" or "vz".
	Backend string `json:"backend"`

	Snapshots     bool `json:"snapshots"`
	VNCConsole    bool `json:"vnc_console"`
	SerialConsole bool `json:"serial_console"`
	Passthrough   bool `json:"passthrough"`
	// UEFIToggle is whether firmware is a choice; always-UEFI reports false.
	UEFIToggle bool `json:"uefi_toggle"`
	TPM        bool `json:"tpm"`
	Bridge     bool `json:"bridge"`
	VLAN       bool `json:"vlan"`
	// CPUTuning is whether CPU model, topology and pinning can be chosen.
	CPUTuning bool `json:"cpu_tuning"`
	HostStats bool `json:"host_stats"`
	// VMStats is whether per-VM CPU and memory usage are measured.
	VMStats bool `json:"vm_stats"`
}

// BackendLibvirt and BackendVZ are the values Capabilities.Backend takes.
const (
	BackendLibvirt = "libvirt"
	BackendVZ      = "vz"
)

// LibvirtCapabilities is what the libvirt backend offers: everything except a
// serial console.
func LibvirtCapabilities() Capabilities {
	return Capabilities{
		Backend:     BackendLibvirt,
		Snapshots:   true,
		VNCConsole:  true,
		Passthrough: true,
		UEFIToggle:  true,
		TPM:         true,
		Bridge:      true,
		VLAN:        true,
		CPUTuning:   true,
		HostStats:   true,
		VMStats:     true,
	}
}

// CapabilitiesOf returns b's own Capabilities when it implements Capable, and
// the libvirt set otherwise, including for a nil b.
func CapabilitiesOf(b Backend) Capabilities {
	if c, ok := b.(Capable); ok {
		return c.Capabilities()
	}
	return LibvirtCapabilities()
}

// ErrUnsupported means the backend cannot do what was asked at all, as
// opposed to failing at it. The API maps it to 501 Not Implemented.
var ErrUnsupported = errors.New("not supported by this hypervisor backend")

// ErrVMNotFound means no VM has the name that was asked for. Both backends
// wrap it, with the name, and the API maps it to 404 Not Found.
var ErrVMNotFound = vm.ErrVMNotFound
